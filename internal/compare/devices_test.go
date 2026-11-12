package compare

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDevices(t *testing.T) {
	dir := t.TempDir()
	head := "WigleWifi-1.6,appRelease=test,model=rig,device=rig\n" +
		"MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,RCOIs,MfgrId,Type\n"
	day1 := head +
		"B4:1E:52:00:11:22,Flock-AB12,[WPA2-PSK-CCMP][ESS],2026-11-01 10:00:00,6,2437,-70,49.0000,-123.0000,0,3,,,WIFI\n" +
		"B4:1E:52:00:11:22,Flock-AB12,[WPA2-PSK-CCMP][ESS],2026-11-01 10:00:05,6,2437,-55,49.0001,-123.0000,0,3,,,WIFI\n" +
		"DA:A1:19:00:00:01,J-Rosella2,Misc [LE],2026-11-01 10:00:10,0,0,-80,49.0002,-123.0000,0,3,,0,BLE\n" +
		"C2:11:22:33:44:55,,Misc [LE],2026-11-01 10:00:15,0,0,-75,49.0003,-123.0000,0,3,,3625,BLE\n" +
		"F0:18:98:00:00:01,HomeNet,[WPA2-PSK-CCMP][ESS],2026-11-01 10:00:20,1,2412,-60,49.0004,-123.0000,0,3,,,WIFI\n"
	day2 := head +
		"B4:1E:52:00:11:22,Flock-AB12,[WPA2-PSK-CCMP][ESS],2026-11-02 10:00:00,6,2437,-65,49.0000,-123.0000,0,3,,,WIFI\n" +
		"DA:A1:19:00:00:02,LVS-Lush3,Misc [LE],2026-11-02 10:00:10,0,0,-82,49.0002,-123.0000,0,3,,0,BLE\n"
	var files []FileIn
	for n, body := range map[string]string{"day1.csv": day1, "day2.csv": day2} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, FileIn{Name: n, Size: int64(len(body)), Path: p})
	}
	var last map[string]any
	e := New(func(m map[string]any) {
		if m["type"] != "progress" {
			last = m
		}
	}, filepath.Join(dir, "tmp"))
	cfg := &Config{Mode: "drive", DriveGapH: 6, LegGapMin: 20, Radius: 25, Typ: "all"}
	e.Handle(&Message{Type: "add", Files: files, Cfg: cfg})
	res := roundTrip(last).(map[string]any)["result"].(map[string]any)
	if n := res["n"].(float64); n != 2 {
		t.Fatalf("drives: %v", n)
	}

	type answer struct {
		Drives []struct {
			Key                         string
			Devices, Notable, Classified int
		}
		Categories []struct {
			Code string
			N    int
			By   map[string]int
		}
		Makers []struct {
			Name string
			N    int
		}
		List []struct {
			MAC, Name, Type, Maker, Category string
			Notable                         bool
			N                               int
			Best                            int
			Drives                          []string
		}
		Matched int
	}
	ask := func(f *DeviceFilter) answer {
		t.Helper()
		e.Handle(&Message{Type: "devices", Cfg: cfg, Filter: f})
		if last["type"] != "devices" {
			t.Fatalf("answer: %v", last)
		}
		b, _ := json.Marshal(last)
		var a answer
		if err := json.Unmarshal(b, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}

	a := ask(nil)
	if len(a.Drives) != 2 || a.Drives[0].Devices != 4 || a.Drives[0].Notable != 3 || a.Drives[1].Devices != 2 || a.Drives[1].Notable != 2 {
		t.Fatalf("drives: %+v", a.Drives)
	}
	cat := map[string]int{}
	for _, c := range a.Categories {
		cat[c.Code] = c.N
	}
	if cat["SURVEILLANCE"] != 1 || cat["ADULTTOY"] != 2 || cat["FLIPPER"] != 1 {
		t.Fatalf("categories: %+v", a.Categories)
	}
	// the list: notable first, the Flock camera on both drives with its best signal
	if a.Matched != 5 || !a.List[0].Notable || a.List[0].MAC != "B4:1E:52:00:11:22" || a.List[0].N != 3 || a.List[0].Best != -55 || len(a.List[0].Drives) != 2 {
		t.Fatalf("list: %+v", a.List)
	}

	if a = ask(&DeviceFilter{Category: "ADULTTOY"}); a.Matched != 2 {
		t.Fatalf("adult toys: %+v", a.List)
	}
	for _, d := range a.List {
		if d.Type != "BLE" || (d.Maker != "JoyHub" && d.Maker != "Lovense") {
			t.Fatalf("toy: %+v", d)
		}
	}
	if a = ask(&DeviceFilter{Maker: "Flipper Zero"}); a.Matched != 1 || a.List[0].Category != "FLIPPER" {
		t.Fatalf("by maker: %+v", a.List)
	}
	if a = ask(&DeviceFilter{Typ: "wifi"}); a.Matched != 2 {
		t.Fatalf("wifi: %+v", a.List)
	}
	if a = ask(&DeviceFilter{Drive: a.Drives[1].Key, Text: "lush"}); a.Matched != 1 || a.List[0].Name != "LVS-Lush3" {
		t.Fatalf("drive and text: %+v", a.List)
	}
	// an excluded drive drops out
	cfg.Excluded = []string{a.Drives[0].Key}
	if a = ask(nil); len(a.Drives) != 1 || a.Matched != 2 {
		t.Fatalf("excluded: %+v %+v", a.Drives, a.List)
	}
	// cleared, nothing is left
	e.Handle(&Message{Type: "clear"})
	if a = ask(nil); len(a.Drives) != 0 || a.Matched != 0 {
		t.Fatalf("cleared: %+v", a)
	}
}
