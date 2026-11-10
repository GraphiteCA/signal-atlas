// Package atlas is Signal Atlas (Graphite's Python, kept in python) in Go: the store, filters and
// queries (survey_data.py), the location model (estimate_ap.py), selection-aware fit groups and
// comparisons (selection_estimates.py, area_estimates.py), the fit job (serve_survey.fit_radio)
//
// The schema, every SQL statement, every validation message and every key Signal Atlas derives
// (radio_key, scope_key, shared:/radio: group hashes) are the Python's, so a store made by the
// Python opens here and the other way round; test/parity checks it result for result.
// What differs is speed: imports parse on every core and write in one transaction with the
// secondary indexes built once at the end, and fits run in parallel.
package atlas

import (
	"math"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
	"signal-atlas/internal/pycsv"
)

// Row is a result row.
type Row = db.Row

// RequiredColumns are the CSV columns Signal Atlas needs (sorted, as its error lists them).
var RequiredColumns = []string{"CurrentLatitude", "CurrentLongitude", "MAC", "RSSI", "SSID"}

// SchemaVersion is PRAGMA user_version of the store.
const SchemaVersion = 5

// SQLiteVersion is the engine's SQLite.
var SQLiteVersion = db.Version()

// ---------------------------------------------------------------------------
// parsing: is_wifi, frequency_mhz, infer_band, parse_observation

// IsWifi is is_wifi(row).
func IsWifi(typ *string) bool {
	t := "WIFI"
	if typ != nil && *typ != "" {
		t = *typ
	}
	return strings.ToUpper(py.Strip(t)) == "WIFI"
}

var freqRe = regexp.MustCompile(`^([\d.]+)(ghz|mhz|khz|hz)?$`)

// FrequencyMHz is frequency_mhz(value); ok is false for Python's None.
func FrequencyMHz(value string) (float64, bool) {
	text := strings.ReplaceAll(strings.ToLower(py.Strip(value)), " ", "")
	if text == "" {
		return 0, false
	}
	m := freqRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	n, err := py.Float(m[1])
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 0, false
	}
	var f float64
	switch m[2] {
	case "ghz":
		f = 1000
	case "mhz":
		f = 1
	case "khz":
		f = .001
	case "hz":
		f = .000001
	default:
		switch {
		case n >= 1e8:
			f = .000001
		case n >= 1e6:
			f = .001
		case n < 100:
			f = 1000
		default:
			f = 1
		}
	}
	return n * f, true
}

// InferBand is infer_band(frequency, explicit): band, its source, and the frequency (nil if none).
func InferBand(frequency, explicit string) (string, string, any) {
	mhz, ok := FrequencyMHz(frequency)
	var measured string
	var fv any
	if ok {
		fv = mhz
		switch {
		case mhz >= 2400 && mhz < 2500:
			measured = "2.4"
		case mhz >= 4900 && mhz < 5925:
			measured = "5"
		case mhz >= 5925 && mhz <= 7125:
			measured = "6"
		default:
			measured = "other"
		}
	}
	declared := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(explicit), "ghz", ""), " ", "")
	if declared != "2.4" && declared != "5" && declared != "6" {
		declared = ""
	}
	if measured != "" && declared != "" && measured != declared {
		return "unknown", "conflicting frequency and explicit band", fv
	}
	if measured != "" {
		return measured, "frequency", fv
	}
	if declared != "" {
		return declared, "explicit band", fv
	}
	// Channel numbers overlap across bands, particularly with 6 GHz. Never guess a band from the
	// channel, the network name, or adjacent MAC addresses.
	return "unknown", "frequency/band unavailable", fv
}

// RadioKey is json.dumps([ssid, mac, band], separators=(',',':'), ensure_ascii=False).
func RadioKey(ssid, mac, band string) string {
	return py.MustDumps([]string{ssid, mac, band}, py.DumpOpts{Compact: true, NoEnsureASCII: true})
}

// Fields is one CSV row by column name; a nil entry is a missing value (Python's None).
type Fields map[string]*string

func (f Fields) get(k string) string {
	if p := f[k]; p != nil {
		return *p
	}
	return ""
}

// Observation is parse_observation's dict.
type Observation struct {
	Key, ID, SSID, MAC                 string
	Latitude, Longitude, RSSI          float64
	Accuracy                           any // float64 or nil
	FirstSeen, DayTable, Date, Channel string
	Frequency                          string
	FrequencyMHz                       any
	Band, BandSource, Source, RadioKey string
}

// Map is the observation as Python's dict.
func (o *Observation) Map() map[string]any {
	return map[string]any{"key": o.Key, "id": o.ID, "ssid": o.SSID, "mac": o.MAC, "latitude": o.Latitude, "longitude": o.Longitude,
		"rssi_dbm": o.RSSI, "accuracy_m": o.Accuracy, "first_seen": o.FirstSeen, "day_table": o.DayTable, "date": o.Date,
		"channel": o.Channel, "frequency": o.Frequency, "frequency_mhz": o.FrequencyMHz, "band": o.Band, "band_source": o.BandSource,
		"source": o.Source, "radio_key": o.RadioKey}
}

func numberOf(f Fields, k string) (float64, bool) {
	p := f[k]
	if p == nil {
		return 0, false
	}
	v, err := py.Float(*p)
	return v, err == nil
}

// ParseObservation is parse_observation(row, line).
func ParseObservation(f Fields, line int64) (*Observation, error) {
	lat, ok1 := numberOf(f, "CurrentLatitude")
	lon, ok2 := numberOf(f, "CurrentLongitude")
	rssi, ok3 := numberOf(f, "RSSI")
	var acc any
	ok4 := true
	if a := f.get("AccuracyMeters"); a != "" {
		var v float64
		v, ok4 = numberOf(f, "AccuracyMeters")
		acc = v
	}
	if !(ok1 && ok2 && ok3 && ok4) {
		return nil, py.Errorf("Invalid numeric measurement on CSV line %d", line)
	}
	fin := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
	if !(fin(lat) && fin(lon) && fin(rssi)) {
		return nil, py.Errorf("Non-finite measurement on CSV line %d", line)
	}
	if !(-85 < lat && lat < 85 && -180 <= lon && lon <= 180 && -150 <= rssi && rssi <= 0) {
		return nil, py.Errorf("Out-of-range coordinates/RSSI on CSV line %d", line)
	}
	if a, ok := acc.(float64); ok && (!fin(a) || a < 0) {
		return nil, py.Errorf("Invalid GPS accuracy on CSV line %d", line)
	}
	mac := strings.ToUpper(py.Strip(f.get("MAC")))
	if mac == "" {
		return nil, py.Errorf("Missing MAC on CSV line %d", line)
	}
	ssid := f.get("SSID")
	explicit := f.get("Band")
	if explicit == "" {
		explicit = f.get("band")
	}
	band, src, mhz := InferBand(f.get("Frequency"), explicit)
	id := f.get("id")
	if id == "" {
		id = itoa(line)
	}
	first := f.get("FirstSeen")
	return &Observation{Key: itoa(line), ID: id, SSID: ssid, MAC: mac, Latitude: lat, Longitude: lon, RSSI: rssi, Accuracy: acc,
		FirstSeen: first, DayTable: f.get("day_table"), Date: py.Head(first, 10), Channel: f.get("Channel"),
		Frequency: f.get("Frequency"), FrequencyMHz: mhz, Band: band, BandSource: src, Source: f.get("source"),
		RadioKey: RadioKey(ssid, mac, band)}, nil
}

func itoa(n int64) string { return py.Str(n) }

// ---------------------------------------------------------------------------
// filters

func likeNeedle(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Bbox parses and checks map bounds "w,s,e,n".
func Bbox(text string) ([4]float64, error) {
	var box [4]float64
	parts := strings.Split(text, ",")
	if len(parts) != 4 {
		for _, s := range parts { // Python converts every part first
			if _, err := py.Float(s); err != nil {
				return box, err
			}
		}
		return box, py.Errorf("Invalid map bounds")
	}
	for i, s := range parts {
		v, err := py.Float(s)
		if err != nil {
			return box, err
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return box, py.Errorf("Invalid map bounds")
		}
		box[i] = v
	}
	w, s, e, n := box[0], box[1], box[2], box[3]
	if !(-180 <= w && w <= e && e <= 180 && -85 <= s && s <= n && n <= 85) {
		return box, py.Errorf("Map bounds must be ordered within longitude ±180°, latitude ±85°")
	}
	return box, nil
}

func minRSSI(p py.Params) (float64, error) {
	m, err := py.Float(p.First("min_rssi", "-150"))
	if err != nil {
		return 0, err
	}
	if math.IsNaN(m) || math.IsInf(m, 0) || !(m >= -150 && m <= 0) {
		return 0, py.Errorf("RSSI must be between -150 and 0 dBm")
	}
	return m, nil
}

type macChoice struct {
	ssid, mode string
	macs       []string
}

// parseMacSelection reads {"ssid": {"mode": ..., "macs": [...]}, ...} keeping the document's order.
func parseMacSelection(text string) ([]macChoice, error) {
	keys, vals, isObj, err := orderedObject(text)
	if err != nil {
		return nil, py.Errorf("Expecting value: %v", err)
	}
	if !isObj || len(keys) > 100 {
		return nil, py.Errorf("MAC selections must be scoped to at most 100 SSIDs")
	}
	out := make([]macChoice, 0, len(keys))
	for i, k := range keys {
		ch, ok := vals[i].(map[string]any)
		mode, _ := ch["mode"].(string)
		if !ok || (mode != "include" && mode != "exclude") {
			return nil, py.Errorf("Invalid SSID/MAC selection mode")
		}
		list, ok := ch["macs"].([]any)
		if !ok {
			return nil, py.Errorf("MAC selections must contain a list of MAC strings")
		}
		macs := make([]string, 0, len(list))
		for _, m := range list {
			s, ok := m.(string)
			if !ok {
				return nil, py.Errorf("MAC selections must contain a list of MAC strings")
			}
			macs = append(macs, s)
		}
		out = append(out, macChoice{k, mode, macs})
	}
	return out, nil
}

// Dates is observation_dates(params).
type Dates struct {
	From, To string
	Dates    []string
}

// Map is the dict Python returns (None for an empty bound).
func (d Dates) Map() map[string]any {
	nv := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	l := d.Dates
	if l == nil {
		l = []string{}
	}
	return map[string]any{"date_from": nv(d.From), "date_to": nv(d.To), "dates": l}
}

func canonical(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch py.ISODateKind(v) {
	case "invalid":
		return "", py.Errorf("Choose valid dates in YYYY-MM-DD format")
	case "format":
		return "", py.Errorf("Choose dates in YYYY-MM-DD format")
	}
	return v, nil
}

// ObservationDates is observation_dates(params).
func ObservationDates(p py.Params) (Dates, error) {
	var d Dates
	var err error
	if d.From, err = canonical(p.First("date_from", "")); err != nil {
		return d, err
	}
	if d.To, err = canonical(p.First("date_to", "")); err != nil {
		return d, err
	}
	if d.From != "" && d.To != "" && d.From > d.To {
		return d, py.Errorf("The start date must be on or before the end date")
	}
	chosen := p.Get("date", nil)
	if len(chosen) > 1000 {
		return d, py.Errorf("Select at most 1,000 dates, or use a date range")
	}
	set := map[string]bool{}
	for _, day := range chosen {
		if day == "" {
			return d, py.Errorf("Select valid dates in YYYY-MM-DD format")
		}
	}
	for _, day := range chosen {
		c, err := canonical(day)
		if err != nil {
			return d, err
		}
		set[c] = true
	}
	for k := range set {
		d.Dates = append(d.Dates, k)
	}
	sort.Strings(d.Dates)
	return d, nil
}

// EstimateScope is estimate_scope(params): the scope dict and its cache key.
func EstimateScope(p py.Params) (map[string]any, string, error) {
	mode := p.First("fit_mode", "all")
	if mode != "all" && mode != "sample" {
		return nil, "", py.Errorf("Fit mode must be all or sample")
	}
	m, err := minRSSI(p)
	if err != nil {
		return nil, "", err
	}
	d, err := ObservationDates(p)
	if err != nil {
		return nil, "", err
	}
	scope := d.Map()
	scope["version"] = int64(2)
	scope["fit_mode"] = mode
	scope["min_rssi"] = m
	return scope, py.MustDumps(scope, py.DumpOpts{Compact: true}), nil
}

// ---------------------------------------------------------------------------
// importing

// Progress is told how an import goes; returning an error cancels it (the import rolls back).
type Progress func(valid, invalid, read, total int64) error

// csvHeader handles csv_reader's header: the WiGLE preamble, the names, the required columns.
type csvHeader struct {
	names     []string
	preamble  bool
	firstLine int64
	done      bool
}

func (h *csvHeader) take(rec []string) (bool, error) {
	if h.done {
		return false, nil
	}
	if !h.preamble && len(rec) > 0 && strings.HasPrefix(strings.ToLower(py.Strip(rec[0])), "wiglewifi-") {
		h.preamble, h.firstLine = true, 3
		return true, nil
	}
	h.preamble = true
	if h.firstLine == 0 {
		h.firstLine = 2
	}
	h.names = make([]string, len(rec))
	have := map[string]bool{}
	for i, n := range rec {
		h.names[i] = py.Strip(n)
		have[h.names[i]] = true
	}
	var missing []string
	for _, c := range RequiredColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	h.done = true
	if len(missing) > 0 {
		return true, py.Errorf("Missing CSV columns: %s", strings.Join(missing, ", "))
	}
	return true, nil
}

// fields maps a record to its header (DictReader: the last of duplicate names wins, a short row's
// missing values are None, extra values are dropped).
func (h *csvHeader) fields(rec []string) Fields {
	f := make(Fields, len(h.names))
	for i, n := range h.names {
		if i < len(rec) {
			v := rec[i]
			f[n] = &v
		} else {
			f[n] = nil
		}
	}
	return f
}

type parsedRow struct {
	obs     *Observation
	line    int64
	nonWifi bool
	err     error
}

// parallelRows runs produce (one goroutine, emitting records in order), parses them on every core
// in batches, and hands the results to write in the original order. An error from any stage stops
// the lot and is returned.
func parallelRows(produce func(emit func([]string, int64) error) error, parse func([]string, int64) parsedRow, write func(parsedRow) error) error {
	const batch = 4096
	type job struct {
		recs  [][]string
		lines []int64
		out   []parsedRow
		done  chan struct{}
	}
	workers := max(1, runtime.NumCPU()-1)
	jobs := make(chan *job, workers*4)
	ordered := make(chan *job, workers*4)
	stop := make(chan struct{})
	var stopOnce sync.Once
	halt := func() { stopOnce.Do(func() { close(stop) }) }
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				j.out = make([]parsedRow, len(j.recs))
				for k := range j.recs {
					j.out[k] = parse(j.recs[k], j.lines[k])
				}
				close(j.done)
			}
		}()
	}
	var prodErr error
	go func() {
		defer close(ordered)
		defer close(jobs)
		cur := &job{done: make(chan struct{})}
		send := func() bool {
			select {
			case jobs <- cur:
			case <-stop:
				return false
			}
			select {
			case ordered <- cur:
			case <-stop:
				return false
			}
			cur = &job{done: make(chan struct{})}
			return true
		}
		prodErr = produce(func(rec []string, line int64) error {
			cur.recs = append(cur.recs, rec)
			cur.lines = append(cur.lines, line)
			if len(cur.recs) == batch && !send() {
				return errStopped
			}
			return nil
		})
		if prodErr == nil && len(cur.recs) > 0 {
			send()
		}
	}()
	var werr error
	for j := range ordered {
		<-j.done
		if werr != nil {
			continue
		}
		for _, r := range j.out {
			if werr = write(r); werr != nil {
				halt()
				break
			}
		}
	}
	halt()
	wg.Wait()
	if werr != nil {
		return werr
	}
	if prodErr != nil && prodErr != errStopped {
		return prodErr
	}
	return nil
}

type stoppedError struct{}

func (stoppedError) Error() string { return "stopped" }

var errStopped error = stoppedError{}

// ReadObservations is estimate_ap.read_observations: one network's observations from a CSV.
func ReadObservations(path string, ssid *string, mac string) ([]*Observation, error) {
	var out []*Observation
	var h csvHeader
	var line int64
	chosen := ssid
	err := pycsv.ReadFile(path, func(rec []string) error {
		if !h.done {
			_, err := h.take(rec)
			if err == nil && h.done {
				line = h.firstLine - 1
			}
			return err
		}
		if len(rec) == 0 {
			return nil
		}
		line++
		f := h.fields(rec)
		if !IsWifi(f["Type"]) {
			return nil
		}
		if mac != "" && strings.ToUpper(py.Strip(f.get("MAC"))) != strings.ToUpper(py.Strip(mac)) {
			return nil
		}
		s := f.get("SSID")
		if ssid != nil && s != *ssid {
			return nil
		}
		if chosen == nil {
			chosen = &s
		} else if s != *chosen {
			return py.Errorf("Select one network with --ssid, or use import_survey.py and serve_survey.py for multiple SSIDs")
		}
		o, err := ParseObservation(f, line)
		if err != nil {
			return err
		}
		out = append(out, o)
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, py.Errorf("No observations matched the selected network/radio")
	}
	return out, nil
}

// PageLimit is page_limit(params, default, maximum).
func PageLimit(p py.Params, dflt, maximum int64) (int64, int64, error) {
	l, err := py.Int(p.First("limit", itoa(dflt)))
	if err != nil {
		return 0, 0, err
	}
	o, err := py.Int(p.First("offset", "0"))
	if err != nil {
		return 0, 0, err
	}
	return max(1, min(maximum, l)), max(0, o), nil
}
