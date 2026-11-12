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
	files, err := a.ExportSurveyDrives(ctx, t.TempDir(), "test", "import", []string{second, first, second, "no-such-import"}, nil)
	if err != nil || len(files) != 2 || files[0].Key != second || files[1].Key != first {
		t.Fatalf("by import: %+v %v", files, err)
	}
	for _, f := range files {
		// the undated row of the second import is left out
		if info, err := os.Stat(f.Path); err != nil || info.Size() != f.Size || dataRows(f.Path) != 2 {
			t.Fatalf("import file: %+v %v", f, err)
		}
	}
	files, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "day", []string{"2026-01-02"}, nil)
	if err != nil || len(files) != 1 || dataRows(files[0].Path) != 4 {
		t.Fatalf("by day: %+v %v", files, err)
	}
	files, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "day", []string{"2030-01-01"}, nil)
	if err != nil || len(files) != 0 {
		t.Fatalf("absent day: %+v %v", files, err)
	}
	if _, err = a.ExportSurveyDrives(ctx, t.TempDir(), "test", "week", []string{"2026-01-02"}, nil); err == nil {
		t.Fatal("unknown grouping accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = a.ExportSurveyDrives(cancelled, t.TempDir(), "test", "day", []string{"2026-01-02"}, nil); err == nil {
		t.Fatal("export ignored cancellation")
	}
	if a.Obs.State().N != 5 {
		t.Fatal("export changed survey")
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
