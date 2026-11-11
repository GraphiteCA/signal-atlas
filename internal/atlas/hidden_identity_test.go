package atlas

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"signal-atlas/internal/py"
)

func hiddenRadio(mac, band string) Group {
	return Group{"radio_key": RadioKey("", mac, band), "ssid": "", "mac": mac, "band": band, "count": int64(8)}
}

func TestHiddenSelectionKeepsThousandMACsIndependent(t *testing.T) {
	groups := make([]Group, 1000)
	for i := range groups {
		groups[i] = hiddenRadio(fmt.Sprintf("02:60:00:00:%02X:%02X", i/256, i%256), "2.4")
	}
	for _, individual := range []string{"0", "1"} {
		got := EstimationGroups(groups, py.Params{"shared": {"1"}, "individual": {individual}})
		if len(got) != len(groups) {
			t.Fatal("hidden radios disappeared or merged", individual, len(got))
		}
		for _, g := range got {
			if len(members(g)) != 0 || gi(g, "count") != 8 {
				t.Fatal("different hidden addresses combined", g)
			}
		}
	}
	if got := JointGroups(groups[:2], py.Params{"shared": {"1"}}); len(got) != 0 {
		t.Fatal("two hidden MACs shared a location", got)
	}
}

func TestHiddenBandsShareOnlyWithinSameMACAndKeepVersionIdentity(t *testing.T) {
	mac := "02:60:00:00:00:01"
	groups := []Group{hiddenRadio(mac, "2.4"), hiddenRadio(mac, "5"), hiddenRadio("02:60:00:00:00:02", "2.4")}
	got := EstimationGroups(groups, py.Params{"shared": {"1"}, "individual": {"0"}})
	if len(got) != 2 {
		t.Fatal(got)
	}
	for _, g := range got {
		for _, m := range members(g) {
			if gs(m, "mac") != mac {
				t.Fatal("hidden bands crossed MAC boundary", g)
			}
		}
		if len(members(g)) > 0 && groupTarget(g) != TargetKey("", mac) {
			t.Fatal("display MAC count used as target identity", g, groupTarget(g))
		}
	}
	// A real, nonempty SSID may literally contain this text. Importers must
	// identify their own hidden sentinels; do not silently rewrite real names.
	for _, g := range groups[:2] {
		g["ssid"] = "(hidden SSID)"
	}
	if got := JointGroups(groups[:2], py.Params{"shared": {"1"}}); len(got) != 1 || gs(got[0], "ssid") != "(hidden SSID)" {
		t.Fatal("literal SSID was normalized without source evidence", got)
	}
}

func TestHiddenPreparationReadsOnlyTargetMAC(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var csv strings.Builder
	csv.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for n := 0; n < 1000; n++ {
		lat, lon := 48.4, -123.4
		if n%2 == 1 {
			lat, lon = 49.4, -124.4
		}
		for i := 0; i < 8; i++ {
			fmt.Fprintf(&csv, "02:61:00:00:%02X:%02X,,2026-11-01,2412,%d,%.7f,%.7f,3,WIFI\n", n/256, n%256, -50-i, lat+float64(i%3)*.00001, lon+float64(i%2)*.00001)
		}
	}
	file := filepath.Join(t.TempDir(), "hidden.csv")
	writeFile(t, file, csv.String())
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	ns := preparationNamespace(t, a, "sample", "shared", true)
	targets, err := a.NextPreparationTargets(context.Background(), ns.ID, 32<<20, 1100)
	if err != nil || len(targets) != 1000 {
		t.Fatal(len(targets), err)
	}
	for _, target := range targets {
		if target.SSID != "" || target.Key != TargetKey("", target.MAC) {
			t.Fatal("hidden target is keyed by name", target)
		}
	}
	for _, target := range targets[:2] {
		prepared, err := a.ComputePreparationTarget(context.Background(), ns, target, 32<<20)
		if err != nil || len(prepared.Writes) != 1 {
			t.Fatal(prepared, err)
		}
		var fit map[string]any
		if err := json.Unmarshal([]byte(prepared.Writes[0].Text), &fit); err != nil || fit["count"] != float64(8) || fit["mac"] != target.MAC || fit["status"] != "fitted" {
			t.Fatal("fit included another hidden AP's observations", target, fit, err)
		}
	}
	_, err = a.FitRadio(Group{"ssid": "", "members": []Group{hiddenRadio(targets[0].MAC, "2.4"), hiddenRadio(targets[1].MAC, "2.4")}}, ns.Params, nil)
	if err == nil || !strings.Contains(err.Error(), "BSSID/MAC") {
		t.Fatal("direct fit accepted multiple hidden MACs", err)
	}
}
