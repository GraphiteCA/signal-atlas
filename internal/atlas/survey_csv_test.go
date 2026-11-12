package atlas

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
)

func duplicateSurvey(t *testing.T) *Atlas {
	t.Helper()
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	header := "MAC,SSID,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"
	row := "02:00:00:00:00:01,\"Cafe, North\",2026-01-02 12:00:00,1,2412,-60,49,-123,3,WIFI\n"
	for i, body := range []string{row + row, row + strings.Replace(row, "-60", "-61", 1) + strings.Replace(row, "2026-01-02 12:00:00", "", 1)} {
		file := filepath.Join(dir, []string{"first.csv", "second.csv"}[i])
		writeFile(t, file, header+body)
		if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Recreate a legacy survey with duplicates; new imports now deduplicate.
	st := a.Obs.State()
	first, second := st.Segs[0], st.Segs[1]
	copyFirst := first.Range(0, 1)
	copyFirst.Base = 2
	overlap := first.Range(0, 1)
	overlap.Base = 3
	overlap.ImportID = second.ImportID
	tail := second.Range(0, second.N)
	tail.Base = 4
	if err := a.Obs.Commit(obs.Change{Segments: []*obs.Segment{first, copyFirst, overlap, tail}}); err != nil {
		t.Fatal(err)
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer put()
	if _, err = db.Exec(c, "UPDATE imports SET active_rows=active_rows+1,valid_rows=valid_rows+1,duplicate_rows=0"); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSurveyDeduplicationPreservesHistoryAndIDs(t *testing.T) {
	a := duplicateSurvey(t)
	ctx := context.Background()
	preview, err := a.DuplicatePreview(ctx)
	if err != nil || preview.Rows != 2 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	candidate, result, err := a.PrepareDeduplication(ctx, filepath.Join(t.TempDir(), "candidate"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if result["removed_rows"] != int64(2) || candidate.Obs.State().N != 3 || a.Obs.State().N != 5 {
		t.Fatalf("counts: %v", result)
	}
	var ids []int64
	for _, s := range candidate.Obs.State().Segs {
		for i := 0; i < s.N; i++ {
			ids = append(ids, s.ID(i))
		}
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 4 || ids[2] != 5 {
		t.Fatalf("survivor IDs: %v", ids)
	}
	rows := table(t, candidate, "SELECT active_rows,valid_rows,zero_removed_rows FROM imports ORDER BY id")
	if len(rows) != 2 || db.Int(rows[0]["active_rows"]) != 1 || db.Int(rows[1]["active_rows"]) != 2 || db.Int(rows[0]["valid_rows"]) != 2 || db.Int(rows[0]["zero_removed_rows"]) != 0 {
		t.Fatalf("history: %v", rows)
	}
	if err = candidate.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	preview, err = candidate.DuplicatePreview(ctx)
	if err != nil || preview.Rows != 0 {
		t.Fatalf("repeat: %v %v", preview, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = a.DuplicatePreview(cancelled); err == nil {
		t.Fatal("preview ignored cancellation")
	}
}

func TestSurveyDrivesForCompare(t *testing.T) {
	a := duplicateSurvey(t)
	ctx := context.Background()
	list, err := a.SurveyDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	days, imports := list["days"].([]Row), list["imports"].([]Row)
	if list["observations"] != int64(5) || list["undated"] != int64(1) || len(days) != 1 || days[0]["key"] != "2026-01-02" || days[0]["rows"] != int64(4) {
		t.Fatalf("days: %v", list)
	}
	if len(imports) != 2 || imports[0]["name"] != "first.csv" || imports[0]["rows"] != int64(2) || imports[1]["rows"] != int64(2) || imports[1]["from"] != "2026-01-02" || imports[1]["to"] != "2026-01-02" {
		t.Fatalf("imports: %v", imports)
	}
	dataRows := func(path string) int {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(string(b), "\n")
		rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
		if err != nil || len(rows) == 0 || rows[0][0] != "MAC" {
			t.Fatalf("%s: %v %v", path, rows, err)
		}
		return len(rows) - 1
	}
	first, second := imports[0]["key"].(string), imports[1]["key"].(string)
	// repeated and unknown keys add nothing; files come back in the order asked for
	files, err := a.ExportSurveyDrives(ctx, t.TempDir(), "test", "import", []string{second, first, second, "no-such-import"}, false, nil)
	if err != nil || len(files) != 2 || files[0].Key != second || files[1].Key != first {
		t.Fatalf("by import: %+v %v", files, err)
	}
	for _, f := range files {
		// the undated row of the second import is left out
		if info, err := os.Stat(f.Path); err != nil || info.Size() != f.Size || dataRows(f.Path) != 2 {
			t.Fatalf("import file: %+v %v", f, err)
		}
	}
	files, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "day", []string{"2026-01-02"}, false, nil)
	if err != nil || len(files) != 1 || dataRows(files[0].Path) != 4 {
		t.Fatalf("by day: %+v %v", files, err)
	}
	files, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "day", []string{"2030-01-01"}, false, nil)
	if err != nil || len(files) != 0 {
		t.Fatalf("absent day: %+v %v", files, err)
	}
	if _, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "week", []string{"2026-01-02"}, false, nil); err == nil {
		t.Fatal("unknown grouping accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = a.ExportSurveyDrives(cancelled, t.TempDir(), "test", "day", []string{"2026-01-02"}, false, nil); err == nil {
		t.Fatal("export ignored cancellation")
	}
	if a.Obs.State().N != 5 {
		t.Fatal("export changed survey")
	}
}

// Exact duplicates are set aside at import but stay on record, so Compare gets them back.
func TestSurveyDrivesPutDuplicatesBack(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	header := "MAC,SSID,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"
	row := func(mac, rssi string) string {
		return "02:00:00:00:00:" + mac + ",Cafe,2026-01-02 12:00:00,1,2412," + rssi + ",49,-123,3,WIFI\n"
	}
	// first.csv holds one row twice; second.csv repeats it and adds one of its own;
	// third.csv holds nothing the survey has not seen
	for name, body := range map[string]string{"first.csv": row("01", "-60") + row("01", "-60") + row("02", "-61"), "second.csv": row("01", "-60") + row("03", "-62"), "third.csv": row("02", "-61")} {
		writeFile(t, filepath.Join(dir, name), header+body)
	}
	for _, name := range []string{"first.csv", "second.csv", "third.csv"} {
		if _, err := a.ImportCSV(filepath.Join(dir, name), false, nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if a.Obs.State().N != 3 {
		t.Fatalf("stored %d", a.Obs.State().N)
	}
	list, err := a.SurveyDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	imports := list["imports"].([]Row)
	if len(imports) != 3 || imports[0]["rows"] != int64(2) || imports[0]["duplicates"] != int64(1) || imports[1]["rows"] != int64(1) || imports[1]["duplicates"] != int64(1) ||
		imports[2]["name"] != "third.csv" || imports[2]["rows"] != int64(0) || imports[2]["duplicates"] != int64(1) {
		t.Fatalf("imports: %v", imports)
	}
	macs := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(string(b), "\n")
		rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows[1:] {
			out = append(out, r[0][len(r[0])-2:])
		}
		return strings.Join(out, " ")
	}
	keys := []string{imports[0]["key"].(string), imports[1]["key"].(string), imports[2]["key"].(string)}
	files, err := a.ExportSurveyDrives(ctx, t.TempDir(), "test", "import", keys, true, nil)
	if err != nil || len(files) != 3 || macs(files[0].Path) != "01 01 02" || macs(files[1].Path) != "01 03" || macs(files[2].Path) != "02" {
		t.Fatalf("by import with duplicates: %+v %v", files, err)
	}
	// without them an import's drive is what it stored, and the third import stored nothing
	files, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "import", keys, false, nil)
	if err != nil || len(files) != 2 || macs(files[0].Path) != "01 02" || macs(files[1].Path) != "03" {
		t.Fatalf("by import as stored: %+v %v", files, err)
	}
	files, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "day", []string{"2026-01-02"}, true, nil)
	if err != nil || len(files) != 1 || macs(files[0].Path) != "01 01 01 02 02 03" {
		t.Fatalf("by day with duplicates: %+v %v", files, err)
	}
	// the survey export itself is unchanged: one row for each stored observation
	n, _, err := a.ExportSurveyCSV(ctx, filepath.Join(dir, "all.csv"), "test", false, nil)
	if err != nil || n != 3 {
		t.Fatalf("survey export: %d %v", n, err)
	}
}

func TestCompareRunsAreSavedWithTheSurvey(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	run := CompareRunInput{Name: "  New antenna  ", Drives: 2, Sources: []byte(`[{"key":"d1","name":"Old"},{"key":"d2","name":"New"}]`), State: []byte(`{"cfg":{"mode":"drive"}}`), Result: []byte(`{"n":2,"roadKm":{"2":12.5}}`)}
	id, err := a.SaveCompareRun(c, run)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	for _, bad := range []CompareRunInput{{Name: " ", Drives: 2, Sources: run.Sources, State: run.State, Result: run.Result}, {Name: "x", Drives: 2, Sources: run.Sources, State: run.State, Result: []byte(`{"n":`)}, {Name: "x", Drives: 0, Sources: run.Sources, State: run.State, Result: run.Result}} {
		if _, err := a.SaveCompareRun(c, bad); err == nil {
			t.Fatalf("saved %+v", bad)
		}
	}
	runs, err := CompareRuns(c)
	if err != nil || len(runs) != 1 || runs[0]["name"] != "New antenna" || db.Int(runs[0]["drives"]) != 2 || len(runs[0]["sources"].([]any)) != 2 {
		t.Fatalf("list: %v %v", runs, err)
	}
	got, err := CompareRun(c, id)
	if err != nil || got == nil || db.Str(got["result_json"]) != string(run.Result) || db.Str(got["state_json"]) != string(run.State) {
		t.Fatalf("read back: %v %v", got, err)
	}
	if missing, err := CompareRun(c, "no-such-run"); err != nil || missing != nil {
		t.Fatalf("missing: %v %v", missing, err)
	}
	put()
	// a portable snapshot carries the runs, and opening it in another survey brings them along
	snapshot := filepath.Join(dir, "snapshot.sqlite3")
	if err := a.ExportSQLite(snapshot, nil); err != nil {
		t.Fatal(err)
	}
	b, err := Open(filepath.Join(dir, "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.OpenDatabase(snapshot, nil, nil); err != nil {
		t.Fatal(err)
	}
	bc, bput, err := b.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bput()
	if runs, err = CompareRuns(bc); err != nil || len(runs) != 1 || runs[0]["id"] != id {
		t.Fatalf("carried: %v %v", runs, err)
	}
	if err = b.DeleteCompareRun(bc, id); err != nil {
		t.Fatal(err)
	}
	if runs, err = CompareRuns(bc); err != nil || len(runs) != 0 {
		t.Fatalf("deleted: %v %v", runs, err)
	}
}

func TestSurveyCSVExports(t *testing.T) {
	a := duplicateSurvey(t)
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "all.csv")
	n, _, err := a.ExportSurveyCSV(ctx, dest, "test", false, nil)
	if err != nil || n != 5 {
		t.Fatalf("single: %d %v", n, err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := strings.Cut(string(b), "\n")
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 || rows[1][1] != "Cafe, North" || rows[1][2] != "" || rows[1][9] != "" || rows[1][5] != "2412" {
		t.Fatalf("CSV: %v", rows)
	}
	days := filepath.Join(dir, "days")
	os.Mkdir(days, 0755)
	n, files, err := a.ExportSurveyCSV(ctx, days, "test", true, nil)
	if err != nil || n != 5 || len(files) != 2 || files[0] != "wigle_2026-01-02.csv" || files[1] != "wigle_undated.csv" {
		t.Fatalf("days: %d %v %v", n, files, err)
	}
	roundtrip, err := Open(filepath.Join(dir, "roundtrip"))
	if err != nil {
		t.Fatal(err)
	}
	defer roundtrip.Close()
	if _, err = roundtrip.ImportCSV(dest, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if roundtrip.Obs.State().N != 3 {
		t.Fatal("roundtrip did not deduplicate observations")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err = a.ExportSurveyCSV(cancelled, filepath.Join(dir, "cancel.csv"), "test", false, nil); err == nil {
		t.Fatal("export ignored cancellation")
	}
	if a.Obs.State().N != 5 {
		t.Fatal("export changed survey")
	}
}
