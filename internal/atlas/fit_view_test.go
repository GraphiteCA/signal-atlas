package atlas

import (
	"fmt"
	"path/filepath"
	"reflect"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
	"strings"
	"testing"
)

func TestBoundedFitViewPreservesSparseSourceIDsAndSamples(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "survey"))
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	var text strings.Builder
	text.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&text, "02:00:00:00:01:01,Sample,2026-11-%02d,2412,%d,%.8f,%.8f,3,WIFI\n", 10+i%2, -40-i%55, 48.4+float64(i%9)*.0001, -123.4+float64(i%13)*.0001)
	}
	file := filepath.Join(t.TempDir(), "source.csv")
	writeFile(t, file, text.String())
	if _, e = a.ImportCSV(file, false, nil, "", nil); e != nil {
		t.Fatal(e)
	}
	p := py.Params{"date": {"2026-11-10"}, "fit_mode": {"sample"}, "solver": {"fast"}}
	groups, e := a.SelectedGroups(p)
	if e != nil || len(groups) != 1 {
		t.Fatal(groups, e)
	}
	source := a.Obs.State()
	seg := obs.NewSegment(1, 1, 400)
	for _, s := range source.Segs {
		for i := 0; i < s.N; i++ {
			if source.Str[s.Date[i]] != "2026-11-10" {
				continue
			}
			row := obs.Row{SSID: s.SSID[i], MAC: s.MAC[i], Band: s.Band[i], BandSrc: s.BandSrc[i], Radio: s.Radio[i], Date: s.Date[i], Lat: s.Lat[i], Lon: s.Lon[i], RSSI: s.RSSI[i], Line: s.Line[i]}
			seg.Append(&row)
			seg.OriginalIDs = append(seg.OriginalIDs, s.ID(i))
		}
	}
	view := &Atlas{Obs: obs.NewReadView([]*obs.Segment{seg}, a.Obs.Dict)}
	expected, e := a.FitRadio(groups[0], p, nil)
	if e != nil {
		t.Fatal(e)
	}
	actual, e := view.FitRadio(groups[0], p, nil)
	if e != nil || !reflect.DeepEqual(expected, actual) {
		t.Fatal("bounded sampler changed fitting results", expected, actual, e)
	}
	if e = view.Obs.Commit(obs.Change{}); e == nil {
		t.Fatal("read-only view allowed a commit")
	}
}
