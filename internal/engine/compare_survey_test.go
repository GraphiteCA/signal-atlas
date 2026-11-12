package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompareTakesDrivesFromSurvey(t *testing.T) {
	s := adminServer(t)
	source := "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n" +
		"02:00:00:00:00:01,One,2026-11-01 10:00:00,2412,-60,49.0000,-123.0000,3,WIFI\n" +
		"02:00:00:00:00:02,Two,2026-11-01 10:00:05,2437,-70,49.0004,-123.0000,3,WIFI\n" +
		"02:00:00:00:00:01,One,2026-11-02 10:00:00,2412,-62,49.0000,-123.0000,3,WIFI\n" +
		"02:00:00:00:00:03,Three,2026-11-02 10:00:05,5180,-71,49.0004,-123.0000,3,WIFI\n"
	native := filepath.Join(t.TempDir(), "two-days.csv")
	if err := os.WriteFile(native, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": []string{native}}))

	w := adminRequest(t, s, "GET", "/api/app/compare/survey", nil, "")
	var list struct {
		Observations int64 `json:"observations"`
		Days         []struct {
			Key  string `json:"key"`
			Rows int64  `json:"rows"`
		} `json:"days"`
		Imports []struct {
			Key, Name string
			Rows      int64
		} `json:"imports"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if list.Observations != 4 || len(list.Days) != 2 || list.Days[0].Key != "2026-11-01" || list.Days[1].Rows != 2 || len(list.Imports) != 1 || list.Imports[0].Name != "two-days.csv" {
		t.Fatalf("list: %s", w.Body.String())
	}

	w = adminRequest(t, s, "POST", "/api/app/compare/new", nil, "")
	var created struct {
		ID string `json:"id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.ID == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	base := "/api/app/compare/" + created.ID
	cfg := map[string]any{"mode": "drive", "driveGapH": 6, "legGapMin": 20, "radius": 25, "typ": "wifi"}

	if w = adminRequest(t, s, "POST", base+"/survey", map[string]any{"by": "day", "keys": []string{}, "cfg": cfg}, ""); w.Code == 200 {
		t.Fatal("an empty choice was accepted")
	}
	if w = adminRequest(t, s, "POST", base+"/survey", map[string]any{"by": "day", "keys": []string{"2026-11-01"}, "cfg": cfg}, "stale-survey"); w.Code != 409 {
		t.Fatal("a stale survey was accepted", w.Code, w.Body.String())
	}
	w = adminRequest(t, s, "POST", base+"/survey", map[string]any{"by": "day", "keys": []string{"2026-11-01", "2026-11-02"}, "cfg": cfg}, s.surveyID)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"drives": 2`) {
		t.Fatal(w.Code, w.Body.String())
	}

	// the engine answers with one drive a day, each a drive of its own
	type drive struct {
		Solo  bool     `json:"solo"`
		Rows  int      `json:"rows"`
		Files []string `json:"files"`
	}
	type message struct {
		Type   string `json:"type"`
		Errors []any  `json:"errors"`
		Result *struct {
			AllDrives []drive `json:"allDrives"`
		} `json:"result"`
	}
	var drives []drive
	after, deadline := int64(0), time.Now().Add(20*time.Second)
	for drives == nil && time.Now().Before(deadline) {
		for _, q := range s.cmp.get(created.ID).next(after, 2*time.Second) {
			var m message
			if err := json.Unmarshal(q.Msg, &m); err != nil {
				t.Fatal(err, string(q.Msg))
			}
			after = q.Seq
			if m.Type == "error" || len(m.Errors) > 0 {
				t.Fatal(string(q.Msg))
			}
			if m.Type == "result" && m.Result != nil {
				drives = m.Result.AllDrives
			}
		}
	}
	if len(drives) != 2 {
		t.Fatalf("drives: %+v", drives)
	}
	for i, want := range []string{"Survey 2026-11-01.csv", "Survey 2026-11-02.csv"} {
		if d := drives[i]; !d.Solo || d.Rows != 2 || len(d.Files) != 1 || d.Files[0] != want {
			t.Fatalf("drive %d: %+v", i, d)
		}
	}

	// the same day again is the engine's "Already added", and an import is a drive too
	w = adminRequest(t, s, "POST", base+"/survey", map[string]any{"by": "import", "keys": []string{list.Imports[0].Key}, "cfg": cfg}, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"drives": 1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.Atlas.Obs.State().N != 4 {
		t.Fatal("compare changed the survey")
	}
}
