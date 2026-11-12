// Package devices works out what a logged network or Bluetooth device probably is: who made it
// (its manufacturer) and what kind of device it is (its category). It reads only what a WiGLE CSV
// row carries: the MAC address, the SSID or BLE name, the type and the BLE manufacturer id (MfgrId).
//
// The rules come from three places, tried in this order:
//
//  1. The detection tables of Warble 3.1.321, as the reverse-engineering report of Nov 9, 2026
//     lists them: MAC prefixes (data/notable_prefixes.tsv), Bluetooth SIG company ids, and name
//     rules (Flock, Flipper Zero, Pineapple, train Wi-Fi, ...). These make a device notable.
//  2. Adult toys, from the buttplug.io device list the report reproduces: BLE name prefixes and
//     596 exact names (data/adult_toy_names.txt). Also notable.
//  3. Everyday categories: the WarHog Insights name rules, the report's BLE company categories
//     (data/ble_categories.tsv) and what a manufacturer usually makes.
//
// Rules that need a BLE service UUID or advertisement payload (Lovense and Kiiroo service UUIDs,
// Apple and Google Find My, Tile and Samsung tags, Remote ID) can't apply: a WiGLE CSV has none.
//
// The manufacturer is a rule's maker label, else the Bluetooth SIG company the MfgrId names
// (data/ble_companies.tsv.gz), else the IEEE owner of the MAC's MA-S, MA-M or MA-L block
// (data/oui.tsv.gz), which a randomised address doesn't have.
package devices

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

//go:embed data/oui.tsv.gz data/ble_companies.tsv.gz data/ble_categories.tsv data/notable_prefixes.tsv data/adult_toy_names.txt
var data embed.FS

// Category is a kind of device. Notable ones are the kinds people go looking for.
type Category struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Group   string `json:"group"`
	Notable bool   `json:"notable"`
}

// Categories in the order the page lists them.
var Categories = []Category{
	{"ADULTTOY", "Adult toys", "Personal", true},
	{"SURVEILLANCE", "Flock / surveillance cams", "Surveillance", true},
	{"TRAFFIC", "ALPR / traffic cams", "Surveillance", true},
	{"CCTV", "CCTV cameras", "Surveillance", true},
	{"SPYCAM", "Hidden / spy cams", "Surveillance", true},
	{"HOMECAM", "Home cameras", "Surveillance", true},
	{"GUNSHOT", "Gunshot sensors", "Surveillance", true},
	{"BODYCAM", "Body cams", "Police and emergency", true},
	{"POLICERADAR", "Police radar", "Police and emergency", true},
	{"LIGHTBAR", "Light bars / sirens", "Police and emergency", true},
	{"EMERGENCYVEH", "Emergency services routers", "Police and emergency", true},
	{"POLICE", "Two-way radios", "Police and emergency", true},
	{"MEDICAL", "Medical (EMS)", "Police and emergency", true},
	{"FORENSIC", "Forensic / intercept gear", "Police and emergency", true},
	{"FLEET", "Fleet cams & cellular routers", "Vehicles", true},
	{"KARR", "KARR immobilizers", "Vehicles", true},
	{"RAIL", "Trains", "Vehicles", true},
	{"DRONE", "Drones", "Gadgets", true},
	{"ACTIONCAM", "Action cams", "Gadgets", true},
	{"GLASSES", "Smart glasses", "Gadgets", true},
	{"FLIPPER", "Flipper Zero", "Hacking and RF tools", true},
	{"RFTOOL", "RF tools", "Hacking and RF tools", true},
	{"HACKTOOL", "Attack tools", "Hacking and RF tools", true},
	{"MESH", "Mesh radios", "Hacking and RF tools", true},
	{"TRACKER", "Trackers", "Trackers", true},
	{"RETAIL", "Retail tracking beacons", "Trackers", true},
	{"HEALTH", "Health devices", "Personal", false},
	{"AUDIO", "Audio", "Personal", false},
	{"WEARABLE", "Wearable / fitness", "Personal", false},
	{"HOTSPOT", "Phone hotspot", "Personal", false},
	{"COMPUTER", "Phone / computer", "Personal", false},
	{"PERIPHERAL", "Peripheral / controller", "Personal", false},
	{"VEHICLE", "Vehicle", "Vehicles", false},
	{"EBIKE", "E-bike / scooter", "Vehicles", false},
	{"CAMERA", "Camera / doorbell", "Home", false},
	{"SMARTHOME", "Smart home / IoT", "Home", false},
	{"LOCK", "Lock / access", "Home", false},
	{"TVGAME", "TV / game console", "Home", false},
	{"PRINTER", "Printer", "Home", false},
	{"HOUSEHOLD", "Household appliance", "Home", false},
	{"BEACON", "Beacon", "Home", false},
	{"ROUTER", "Router / access point", "Networks", false},
	{"ISP", "ISP router", "Networks", false},
	{"PUBLIC", "Public / guest Wi-Fi", "Networks", false},
	{"VENUE", "Business / venue", "Networks", false},
	{"EXTENDER", "Mesh / extender", "Networks", false},
	{"HIDDEN", "Hidden network", "Unclassified", false},
	{"NETWORK", "Other network", "Unclassified", false},
	{"NAMEDBLE", "Other named device", "Unclassified", false},
	{"RANDOMBLE", "Unnamed random device", "Unclassified", false},
	{"UNKNOWN", "Unclassified device", "Unclassified", false},
}

var catByCode = func() map[string]Category {
	m := map[string]Category{}
	for _, c := range Categories {
		m[c.Code] = c
	}
	return m
}()

// Lookup answers a category by its code.
func Lookup(code string) Category { return catByCode[code] }

// Result is what Classify works out for one device.
type Result struct {
	Maker    string // manufacturer, or "" when nothing says
	Category string // a code from Categories
	Detail   string // what the rule found, e.g. "Penguin battery" or a toy's brand
	Notable  bool
	Random   bool // a locally administered (randomised) address
}

// ---- the tables, loaded on first use ----

type prefixRule struct{ maker, code string }

var (
	loadOnce  sync.Once
	oui       map[string]string // 6, 7 or 9 uppercase hex digits
	bleFull   map[int]string
	bleCat    map[int]string
	prefixes  map[string]prefixRule // keys of 8, 10 or 13 characters
	toyNames  map[string]bool
	loadError error
)

func load() {
	loadOnce.Do(func() {
		oui, bleFull, bleCat, prefixes, toyNames = map[string]string{}, map[int]string{}, map[int]string{}, map[string]prefixRule{}, map[string]bool{}
		if err := readGzipTable("data/oui.tsv.gz", func(k, v string) {
			if v != "Private" {
				oui[k] = v
			}
		}); err != nil {
			loadError = err
		}
		if err := readGzipTable("data/ble_companies.tsv.gz", func(k, v string) {
			if id, err := strconv.Atoi(k); err == nil {
				bleFull[id] = v
			}
		}); err != nil && loadError == nil {
			loadError = err
		}
		readLines("data/ble_categories.tsv", func(l string) {
			if p := strings.Split(l, "\t"); len(p) == 2 {
				if id, err := strconv.ParseInt(p[0], 16, 32); err == nil {
					if code, ok := makerCatCodes[p[1]]; ok {
						bleCat[int(id)] = code
					}
				}
			}
		})
		readLines("data/notable_prefixes.tsv", func(l string) {
			if p := strings.Split(l, "\t"); len(p) == 3 {
				prefixes[p[0]] = prefixRule{p[1], p[2]}
			}
		})
		readLines("data/adult_toy_names.txt", func(l string) { toyNames[l] = true })
	})
}

// LoadError reports a bundled table that couldn't be read (classification then runs without it).
func LoadError() error { load(); return loadError }

func readLines(name string, fn func(string)) {
	b, err := data.ReadFile(name)
	if err != nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && l[0] != '#' {
			fn(l)
		}
	}
}

func gunzip(name string) ([]byte, error) {
	b, err := data.ReadFile(name)
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	return io.ReadAll(z)
}

// readGzipTable reads a gzipped "key<tab>value" table, skipping # comments.
func readGzipTable(name string, fn func(k, v string)) error {
	b, err := gunzip(name)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := sc.Text()
		i := strings.IndexByte(l, '\t')
		if i < 1 || l[0] == '#' {
			continue
		}
		if v := strings.TrimSpace(l[i+1:]); v != "" {
			fn(l[:i], v)
		}
	}
	return sc.Err()
}

// The report's company categories, in this package's codes.
var makerCatCodes = map[string]string{
	"Smart home": "SMARTHOME", "Vehicle": "VEHICLE", "Health monitor": "HEALTH", "Glucose monitor": "HEALTH",
	"Headphones": "AUDIO", "Speaker": "AUDIO", "Fitness": "WEARABLE", "Smart ring": "WEARABLE", "Printer": "PRINTER",
	"Camera": "CAMERA", "E-bike / scooter": "EBIKE", "Tracker": "TRACKER", "Controller": "PERIPHERAL", "TV": "TVGAME",
	"Game console": "TVGAME", "Toothbrush": "HOUSEHOLD", "Robot vacuum": "HOUSEHOLD", "Computer": "COMPUTER",
	"Drone": "DRONE", "Beacon": "RETAIL", "IoT / DIY": "SMARTHOME",
}

// OUIName answers the IEEE owner of a MAC ("AA:BB:CC:..." in either case), or "": its 36-bit
// MA-S block, else its 28-bit MA-M block, else its 24-bit MA-L block.
func OUIName(mac string) string {
	load()
	h := strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(mac))
	for _, n := range []int{9, 7, 6} {
		if len(h) >= n {
			if v, ok := oui[h[:n]]; ok {
				return v
			}
		}
	}
	return ""
}

// BLECompany answers the Bluetooth SIG company a manufacturer id names, or "".
func BLECompany(id int) string { load(); return bleFull[id] }

// IsRandom reports a locally administered address: the second bit of the first byte.
func IsRandom(mac string) bool {
	return len(mac) > 1 && strings.ContainsRune("2367abefABEF", rune(mac[1]))
}

// ---- tier 1: Bluetooth SIG company ids ----

type idRule struct{ code, maker string }

var companyRules = map[int]idRule{
	0x0E29: {"FLIPPER", "Flipper Zero"},
	0x034D: {"BODYCAM", "Axon"}, // TASER International
	0x04EC: {"POLICE", "Motorola Solutions"},
	0x0475: {"POLICE", "Icom"},
	0x01AB: {"GLASSES", "Meta"}, 0x058E: {"GLASSES", "Meta"},
	0x0D53: {"GLASSES", "Luxottica"}, 0x060C: {"GLASSES", "Vuzix"},
	0x01B5: {"HOMECAM", "Nest"}, 0x0870: {"HOMECAM", "Wyze"}, 0x0C19: {"HOMECAM", "Arlo"},
	0x06B1: {"HOMECAM", "SimpliSafe"}, 0x06C1: {"HOMECAM", "Alarm.com"}, 0x0DE8: {"HOMECAM", "Vivint"},
	0x0D5B: {"CCTV", "Axis"}, 0x0E25: {"CCTV", "Hikvision"},
	0x0275: {"FLEET", "Geotab"}, 0x036E: {"FLEET", "Motive"}, 0x0862: {"FLEET", "SmartDrive"}, 0x0A65: {"FLEET", "Lytx"},
	0x0576: {"TRACKER", "Globalstar"},
	0x015D: {"RETAIL", "Estimote"}, 0x01FD: {"RETAIL", "Kontakt.io"}, 0x008C: {"RETAIL", "Gimbal"},
	0x00D0: {"HEALTH", "Dexcom"}, 0x03BB: {"HEALTH", "Abbott"}, 0x0282: {"HEALTH", "Phonak"},
	0x0067: {"HEALTH", "GN Hearing"}, 0x0089: {"HEALTH", "GN Hearing"}, 0x00BA: {"HEALTH", "Starkey"},
	0x0107: {"HEALTH", "Oticon"}, 0x01E0: {"HEALTH", "Widex"}, 0x0295: {"HEALTH", "Signia"},
	0x08AA: {"DRONE", "DJI"},
	0x02F2: {"ACTIONCAM", "GoPro"}, 0x10D7: {"ACTIONCAM", "Insta360"},
}

// ---- tier 2: names ----

type nameRule struct {
	code, maker, detail string
	re                  *regexp.Regexp
	has                 []string // or any of these substrings
	only                int      // 0 any, 1 Wi-Fi only, 2 BLE only
}

func rx(s string) *regexp.Regexp { return regexp.MustCompile(s) }

var notableNames = []nameRule{
	{code: "SURVEILLANCE", maker: "Flock", re: rx(`^flock([-_][0-9a-z:]{2,}|\s+[0-9a-f:]{2,}|\s+camera\b.*|[0-9]+)?$`)},
	{code: "SURVEILLANCE", maker: "Flock", detail: "Penguin battery", re: rx(`^penguin[-_ ][0-9a-z]*[0-9][0-9a-z]*$`)},
	{code: "SURVEILLANCE", maker: "Flock", has: []string{"pigvision"}},
	{code: "SURVEILLANCE", maker: "Flock", detail: "External battery", has: []string{"fs ext battery"}},
	// the report's "axon" substring, kept off the end of a word (Saxony, Saxon Court)
	{code: "BODYCAM", maker: "Axon", re: rx(`(^|[^a-z])axon`)},
	{code: "BODYCAM", maker: "Utility", detail: "BodyWorn", has: []string{"bodyworn"}},
	{code: "FLIPPER", maker: "Flipper Zero", has: []string{"flipper"}},
	{code: "HACKTOOL", maker: "WiFi Pineapple", re: rx(`^pineapple([-_][0-9a-z]{1,8}|\s+[0-9a-f]{2,8}|\s+mark\s+[0-9ivx]+)?$`)},
	{code: "HACKTOOL", maker: "Pwnagotchi", has: []string{"pwnagotchi"}},
	{code: "HACKTOOL", maker: "USBKill", has: []string{"usbkill", "usb killer"}},
	{code: "HACKTOOL", maker: "Deauther", has: []string{"deauth", "pwned"}},
	{code: "HACKTOOL", detail: "Rogue Pro", has: []string{"rogue pro"}},
	{code: "RFTOOL", maker: "PandwaRF", has: []string{"pandwarf"}},
	{code: "RFTOOL", maker: "Evil Crow", has: []string{"evil crow"}},
	{code: "RFTOOL", maker: "O.MG Cable", has: []string{"o.mg"}},
	{code: "RFTOOL", maker: "Wi-Fi Duck", has: []string{"wifiduck"}},
	{code: "RFTOOL", maker: "ESP32 Marauder", has: []string{"marauderota"}},
	{code: "MESH", maker: "Meshtastic", has: []string{"meshtastic"}},
	{code: "MESH", maker: "MeshCore", has: []string{"meshcore"}},
	{code: "RAIL", maker: "Amtrak", has: []string{"amtrak"}},
	{code: "RAIL", maker: "Brightline", has: []string{"brightline"}},
	{code: "RAIL", maker: "Eurostar", has: []string{"eurostar"}},
	{code: "RAIL", maker: "SNCF", has: []string{"sncf", "ouifi"}},
	{code: "RAIL", maker: "VIA Rail", has: []string{"via rail", "via_rail", "viarail"}},
	{code: "RAIL", detail: "Train Wi-Fi", re: rx(`\btrain[-_ ]?wi-?fi\b|\bwi-?fi[-_ ]?in[-_ ]?trains?\b`)},
	// "DR", "QT" or "SWDS" and a serial: the report gives the shape, not the exact length check
	{code: "KARR", maker: "KARR", re: rx(`^(dr|qt|swds)[-_ ]?[0-9][0-9a-z]{4,}$`), only: 2},
	// the IPC-xxxx hotspot a generic IP camera raises before it is set up
	{code: "SPYCAM", detail: "Camera setup hotspot", re: rx(`^ipc[-_]?[0-9a-f]{4,}`), only: 1},
	{code: "SPYCAM", detail: "Name looks like a hidden camera", has: []string{"spy cam", "spycam", "mini cam", "nanny cam"}},
}

// Adult toys: buttplug.io's BLE name prefixes, then exact names (data/adult_toy_names.txt).
var toyPrefixes = []struct{ prefix, brand string }{
	{"lvs-", "Lovense"}, {"love-", "Lovense"},
	{"ohd_hw0_", "OhMiBod"}, {"ohd_hw1_", "OhMiBod"}, {"ohd_hw2_", "OhMiBod"}, {"ohd_hw3_", "OhMiBod"}, {"ohd_hw4_", "OhMiBod"},
	{"fm-", "Fun Factory"}, {"sf", "Satisfyer"}, {"funcode_", "Cueme"}, {"ccyb_", "Xibao"}, {"m1_", "Fredorch"},
	{"vx001_", "Youou"}, {"x-ring", "SayberX"}, {"eropair", "Hismith"}, {"aogu ble", "Pretty Love"},
}

// The brand behind an exact name, where the name's family says (the report's "recognizable families").
var toyBrands = []struct {
	re    *regexp.Regexp
	brand string
}{
	{rx(`^j-|^joyhub`), "JoyHub"},
	{rx(`^(g[0-9]{2}[0-9a-z]|gx[0-9]{2}|k1[0-9]{2}|k1[0-9][a-z])$`), "Lovehoney"},
	{rx(`^(hpb-|honey-)`), "Honey Play Box"},
	{rx(`^tklm-`), "Luvmazer"},
	{rx(`^ohmibod`), "OhMiBod"},
	{rx(`^picobong`), "PicoBong"},
	{rx(`^sakuraneko`), "Sakuraneko"},
	{rx(`^zalo`), "ZALO"},
	{rx(`^pink_?punch`), "Pink Punch"},
	{rx(`^ly[0-9]+[ab]01$`), "Leten"},
	{rx(`^hismith`), "Hismith"},
	{rx(`^(keon|onyx|pearl)`), "Kiiroo"},
	{rx(`^mv (crescendo|poco|tenuto)$`), "MysteryVibe"},
	{rx(`(^|[ ])(alex|ava|amn|ella|emma|hannes|iker|mora|phoenix|sam|trysta|vick) neo\b`), "Svakom"},
	{rx(`_vibio$`), "Vibio"},
	{rx(`^nobra`), "Nobra"}, {rx(`^utimi$`), "Utimi"}, {rx(`^lioness`), "Lioness"}, {rx(`^ossm$`), "OSSM"},
	{rx(`^the handy$`), "The Handy"}, {rx(`^the cowgirl$|^the unicorn$`), "The Cowgirl"},
	{rx(`^twerking ?butt$`), "Twerking Butt"}, {rx(`^youcups$`), "Youcups"}, {rx(`^je joue$`), "Je Joue"},
	{rx(`^libo$`), "LiBo"}, {rx(`^monster(pub|hub)$`), "MonsterPub"}, {rx(`^meese`), "Meese"},
	{rx(`^sayberx$`), "SayberX"}, {rx(`^serveu$`), "ServeU"}, {rx(`^synchro`), "Synchro"},
	{rx(`^vibratissimo$`), "Vibratissimo"}, {rx(`^vorze`), "Vorze"}, {rx(`^we-vibe`), "We-Vibe"},
	{rx(`^love_nuts$`), "Love Nuts"}, {rx(`^realov`), "Realov"}, {rx(`^tryfun`), "TryFun"},
}

func toyBrand(name string) (bool, string) {
	for _, p := range toyPrefixes {
		if strings.HasPrefix(name, p.prefix) {
			return true, p.brand
		}
	}
	if !toyNames[name] {
		return false, ""
	}
	for _, b := range toyBrands {
		if b.re.MatchString(name) {
			return true, b.brand
		}
	}
	return true, ""
}

// Emergency services routers: a Cradlepoint or Sierra Wireless whose network name has one of these words.
var emergencyWords = map[string]string{
	"police": "Police", "pd": "Police", "sheriff": "Police", "trooper": "Police", "patrol": "Police", "marshal": "Police", "chp": "Police",
	"fire": "Fire", "fd": "Fire", "cdf": "Fire", "ocfa": "Fire",
	"ems": "EMS", "medic": "EMS", "rescue": "EMS", "ambulance": "EMS",
}

// tokens splits a lowercase name at anything but letters and digits, and where letters meet digits.
func tokens(s string) []string {
	var out []string
	cur := []byte{}
	kind := 0 // 1 letter, 2 digit
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		k := 0
		if c >= 'a' && c <= 'z' {
			k = 1
		} else if c >= '0' && c <= '9' {
			k = 2
		}
		if k == 0 || (kind != 0 && k != kind) {
			flush()
		}
		if k != 0 {
			cur = append(cur, c)
		}
		kind = k
	}
	flush()
	return out
}

// ---- tier 3: everyday categories (the WarHog Insights rules; RE2 has no lookahead, so the
// brand rules end in (?:[^a-z]|$) where Insights uses (?![a-z])) ----

const carBrands = `tesla|onstar|uconnect|mazda|toyota|honda|ford|chevy|chevrolet|gmc|jeep|dodge|ram|bmw|audi|mercedes|volvo|subaru|kia|hyundai|nissan|lexus|rivian|lucid|polestar|cadillac|buick|chrysler|lincoln|acura|infiniti|vw|volkswagen`
const myCar = `^my\s?(gmc|chevrolet|chevy|buick|cadillac|chrysler|dodge|jeep|ram|lincoln|ford|hyundai|kia|toyota|honda|nissan|mazda|subaru|vw|volkswagen|audi|bmw|mercedes|volvo|lexus|acura|infiniti|tesla)(?:[^a-z]|$)`

type catRule struct {
	code string
	re   *regexp.Regexp
}

var wifiRules = []catRule{
	{"HOTSPOT", rx(`^(iphone|android ?(ap)?|galaxy|pixel|oneplus|redmi|xiaomi|moto ?\w*|sm-[a-z0-9]+|huawei ?\w*|honor|oppo|vivo)\b|'s (iphone|phone|galaxy|pixel)|\bmifi\b|jetpack`)},
	{"PRINTER", rx(`^direct-[0-9a-z]{2}-(hp|epson|canon|brother|xerox|lexmark|ricoh)|\b(printer|laserjet|officejet|deskjet|pixma)\b|^hp-?print|^epson|^brother`)},
	{"CAMERA", rx(`\b(arlo|wyze|blink|reolink|hikvision|ezviz|eufy|doorbell|ipcam|camera)\b|^ring[-_ ]?|^cam[-_]?\d`)},
	{"SMARTHOME", rx(`^(esp[-_]|tasmota|shelly|tuya|smartlife|yeelight|sonos|roku|chromecast|lifx|hue|lutron|ecobee|myq|govee|meross|kasa|wiz[-_]|nanoleaf)`)},
	{"VEHICLE", rx(`\b(carplay|android ?auto|` + carBrands + `)(?:[^a-z]|$)|` + myCar + `|^sync[_ -]`)},
	{"TVGAME", rx(`\b(xbox|playstation|ps[345]|nintendo|smart ?tv|bravia|vizio|firetv|fire ?tv|appletv|apple ?tv)\b`)},
	{"PUBLIC", rx(`(guest|public|\bfree\b|xfinitywifi|attwifi|cablewifi|optimumwifi|starbucks|mcdonald|airport|lobby|visitor)`)},
	{"ISP", rx(`^(att\d{2,}|xfinity|comcast|spectrum|cox\d?|verizon|fios[-_]|centurylink|frontier|optimum|mediacom|windstream|netgear\d*|linksys\d*|tp-?link_|d-?link|asus|belkin|arris|orbi|eero|ubee|bthub|virginmedia|vodafone|talktalk|plusnet|telstra|optus|telus\d+|rogers\d+|bell\d+|shaw)`)},
	{"VENUE", rx(`\b(cafe|coffee|bar|pub|grill|diner|restaurant|pizza|dental|clinic|hospital|school|church|library|office|store|shop|hotel|gym|salon|bakery|market|bank|inn|motel)\b`)},
	{"EXTENDER", rx(`(_ext|-ext|extender|repeater|\bmesh\b)`)},
}

var bleRules = []catRule{
	{"TRACKER", rx(`\b(airtag|tile|smarttag|chipolo|findmy|find my|pebblebee)\b`)},
	{"AUDIO", rx(`(airpods|buds|earbud|earphone|headphone|headset|beats|jbl|bose|sony (wh|wf)|soundcore|speaker|soundbar|\becho\b|homepod|marshall|megaboom|sennheiser|jabra|skullcandy)`)},
	{"WEARABLE", rx(`\b(watch|fitbit|garmin|whoop|oura|mi ?band|galaxy ?fit|amazfit|forerunner|charge ?\d|versa|band ?\d)\b`)},
	{"TVGAME", rx(`\b(tv|bravia|roku|fire ?tv|chromecast|apple ?tv|vizio)\b`)},
	{"PERIPHERAL", rx(`\b(keyboard|mouse|trackpad|mx ?(master|keys|anywhere)|logi|stylus|controller|gamepad|joy-?con)\b`)},
	{"LOCK", rx(`\b(lock|august|yale|schlage|kwikset|nuki|key ?fob)\b`)},
	{"SMARTHOME", rx(`\b(bulb|lamp|light|led|hue|govee|nanoleaf|thermostat|plug|sensor|blind|shade)\b`)},
	{"BEACON", rx(`\b(beacon|ibeacon|eddystone|kontakt|estimote|radbeacon)\b`)},
	{"VEHICLE", rx(`\b(` + carBrands + `)(?:[^a-z]|$)|\b(car ?kit|my ?car|obd)\b|` + myCar + `|^sync[_ -]`)},
	{"HEALTH", rx(`\b(glucose|dexcom|libre|omnipod|cgm|oximeter|thermometer)\b`)},
	{"CAMERA", rx(`\b(camera|cam)\b`)},
}

var vendorRules = []catRule{
	{"PRINTER", rx(`hewlett|canon|brother|epson|xerox|lexmark|ricoh|kyocera|konica`)},
	{"WEARABLE", rx(`garmin|fitbit|fossil|polar electro|suunto|huami|amazfit|whoop|oura`)},
	{"AUDIO", rx(`\bbose\b|sonos|harman|\bjbl\b|sennheiser|jabra|plantronics|skullcandy|bang & olufsen`)},
	{"TRACKER", rx(`\btile\b|chipolo`)},
	{"PERIPHERAL", rx(`logitech|razer|corsair|steelseries`)},
	{"SMARTHOME", rx(`espressif|tuya|shelly|signify|philips lighting|\bring\b|wyze|ecobee|sonoff|itead|lifx|lutron|amazon technologies`)},
	{"VEHICLE", rx(`tesla|ford motor|general motors|toyota|honda|bmw|volkswagen|daimler|hyundai|kia|nissan|volvo|visteon|continental automotive|harman becker`)},
	{"ROUTER", rx(`netgear|tp-?link|d-?link|linksys|asustek|ubiquiti|cisco|meraki|aruba|ruckus|arris|ubee|zyxel|huawei technologies|ruijie|mikrotik|eero|commscope|technicolor|sagemcom|actiontec|calix|zte|sercomm|askey|hitron|cambium|engenius|fortinet|sophos|juniper|extreme networks|peplink|teltonika|sierra wireless|inseego|novatel|franklin wireless|plume|nokia`)},
	{"COMPUTER", rx(`apple|samsung|google|microsoft|lenovo|dell|intel|oneplus|xiaomi|oppo|vivo|motorola|lg electronics|sony mobile|\bhtc\b|realme|nothing technology|asus|acer|hp inc`)},
}

// Chips the report says are common in cheap cameras (Tuya and Sonix left out: they're in far more).
var cameraChips = rx(`anyka|fullhan|goke|ingenic|xiongmai`)

// Classify works out a device's manufacturer and category. mac is "AA:BB:CC:DD:EE:FF" in either
// case, name its SSID or BLE name, ble true for a Bluetooth row, mfgr its MfgrId (-1 for none).
func Classify(mac, name string, ble bool, mfgr int) Result {
	load()
	r := Result{Random: IsRandom(mac)}
	lmac := strings.ToLower(mac)
	lname := strings.ToLower(strings.TrimSpace(name))
	set := func(code, maker, detail string) Result {
		r.Category, r.Notable = code, Lookup(code).Notable
		if maker != "" {
			r.Maker = maker
		}
		if detail != "" {
			r.Detail = detail
		}
		return r
	}
	// the generic manufacturer first: a rule's own maker label replaces it
	if ble && mfgr >= 0 {
		r.Maker = bleFull[mfgr]
	}
	if r.Maker == "" && !r.Random {
		r.Maker = OUIName(mac)
	}

	// 1. the notable prefixes: 36-bit, 28-bit, then 24-bit
	for _, n := range []int{13, 10, 8} {
		if len(lmac) < n {
			continue
		}
		p, ok := prefixes[lmac[:n]]
		if !ok {
			continue
		}
		if p.maker == "Cradlepoint" || p.maker == "Sierra Wireless" {
			for _, t := range tokens(lname) {
				if w, ok := emergencyWords[t]; ok {
					return set("EMERGENCYVEH", p.maker, w+" vehicle router")
				}
			}
			if p.maker == "Cradlepoint" {
				// a Cradlepoint is notable only with an emergency-services name
				set("FLEET", p.maker, "Cellular router")
				r.Notable = false
				return r
			}
		}
		return set(p.code, p.maker, "")
	}
	// 2. the notable Bluetooth companies
	if ble && mfgr >= 0 {
		if c, ok := companyRules[mfgr]; ok {
			return set(c.code, c.maker, "")
		}
	}
	// 3. names
	if lname != "" {
		for _, nr := range notableNames {
			if (nr.only == 1 && ble) || (nr.only == 2 && !ble) {
				continue
			}
			hit := nr.re != nil && nr.re.MatchString(lname)
			for _, h := range nr.has {
				hit = hit || strings.Contains(lname, h)
			}
			if hit {
				return set(nr.code, nr.maker, nr.detail)
			}
		}
		if ble {
			if ok, brand := toyBrand(lname); ok {
				return set("ADULTTOY", brand, "")
			}
		}
	}
	lmaker := strings.ToLower(r.Maker)
	if m := cameraChips.FindString(lmaker); m != "" {
		return set("CAMERA", "", strings.ToUpper(m[:1])+m[1:]+" chip, common in cheap cameras")
	}
	// 4. everyday categories
	if lname != "" {
		rules := wifiRules
		if ble {
			rules = bleRules
		}
		for _, c := range rules {
			if c.re.MatchString(lname) {
				return set(c.code, "", "")
			}
		}
	}
	if ble && mfgr >= 0 {
		if code, ok := bleCat[mfgr]; ok {
			return set(code, "", "")
		}
	}
	if lmaker != "" {
		for _, c := range vendorRules {
			if c.re.MatchString(lmaker) {
				return set(c.code, "", "")
			}
		}
	}
	switch {
	case !ble && lname == "":
		return set("HIDDEN", "", "")
	case !ble:
		return set("NETWORK", "", "")
	case lname != "":
		return set("NAMEDBLE", "", "")
	case r.Random:
		return set("RANDOMBLE", "", "")
	}
	return set("UNKNOWN", "", "")
}
