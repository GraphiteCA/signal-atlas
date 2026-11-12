package devices

import "testing"

func TestTablesLoad(t *testing.T) {
	if err := LoadError(); err != nil {
		t.Fatal(err)
	}
	// IEEE MA-L, MA-M and MA-S together (54,438 when built), less the Private ones
	if len(oui) < 50000 || len(bleFull) < 4000 || len(bleCat) != 185 || len(prefixes) != 328 || len(toyNames) != 596 {
		t.Fatalf("oui %d ble %d bleCat %d prefixes %d toys %d", len(oui), len(bleFull), len(bleCat), len(prefixes), len(toyNames))
	}
	for _, c := range Categories {
		if c.Code == "" || c.Name == "" || c.Group == "" {
			t.Fatalf("category %+v", c)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		mac, name string
		ble       bool
		mfgr      int
		code      string
		maker     string
		notable   bool
	}{
		// MAC prefixes: 24-bit, 28-bit and 36-bit blocks
		{"B4:1E:52:00:11:22", "", false, -1, "SURVEILLANCE", "Flock", true},
		{"F8:4D:FC:01:02:03", "IPC", false, -1, "CCTV", "Hikvision", true},
		{"1C:82:59:D1:00:00", "", false, -1, "POLICERADAR", "Stalker", true},
		{"70:B3:D5:71:F0:01", "", false, -1, "FORENSIC", "GrayKey", true},
		{"0C:FA:22:00:00:01", "", true, -1, "FLIPPER", "Flipper Zero", true},
		// Cradlepoint: notable only with an emergency-services name
		{"00:30:44:12:34:56", "Engine 7 FD", false, -1, "EMERGENCYVEH", "Cradlepoint", true},
		{"00:30:44:12:34:56", "IBR900-firewall", false, -1, "FLEET", "Cradlepoint", false},
		{"00:A0:D5:12:34:56", "Unit12 Sheriff", false, -1, "EMERGENCYVEH", "Sierra Wireless", true},
		// Bluetooth company ids
		{"C2:11:22:33:44:55", "", true, 0x0E29, "FLIPPER", "Flipper Zero", true},
		{"D4:11:22:33:44:55", "", true, 0x034D, "BODYCAM", "Axon", true},
		{"D4:11:22:33:44:55", "", true, 0x00D0, "HEALTH", "Dexcom", false},
		// names
		{"DA:A1:19:00:00:01", "Flock-AB12CD", false, -1, "SURVEILLANCE", "Flock", true},
		{"DA:A1:19:00:00:01", "Penguin-7XQ", true, -1, "SURVEILLANCE", "Flock", true},
		{"DA:A1:19:00:00:01", "Pineapple_5A1F", false, -1, "HACKTOOL", "WiFi Pineapple", true},
		{"DA:A1:19:00:00:01", "Flipper Zero Nibbler", true, -1, "FLIPPER", "Flipper Zero", true},
		{"DA:A1:19:00:00:01", "Meshtastic_a1b2", true, -1, "MESH", "Meshtastic", true},
		{"DA:A1:19:00:00:01", "Amtrak_WiFi", false, -1, "RAIL", "Amtrak", true},
		{"DA:A1:19:00:00:01", "IPC-1A2B3C", false, -1, "SPYCAM", "", true},
		{"DA:A1:19:00:00:01", "Axon Body 3", true, -1, "BODYCAM", "Axon", true},
		// adult toys: a prefix, an exact name with a known family, an exact name without one
		{"DA:A1:19:00:00:01", "LVS-Lush3", true, -1, "ADULTTOY", "Lovense", true},
		{"DA:A1:19:00:00:01", "J-Rosella2", true, -1, "ADULTTOY", "JoyHub", true},
		{"DA:A1:19:00:00:01", "Emma Neo 2", true, -1, "ADULTTOY", "Svakom", true},
		{"DA:A1:19:00:00:01", "Keon", true, -1, "ADULTTOY", "Kiiroo", true},
		{"DA:A1:19:00:00:01", "flamingo", true, -1, "ADULTTOY", "", true},
		// everyday categories
		{"DA:A1:19:00:00:01", "myChevrolet 7508", false, -1, "VEHICLE", "", false},
		{"DA:A1:19:00:00:01", "DIRECT-4F-HP OfficeJet", false, -1, "PRINTER", "", false},
		{"DA:A1:19:00:00:01", "", false, -1, "HIDDEN", "", false},
		{"DA:A1:19:00:00:01", "", true, -1, "RANDOMBLE", "", false},
		{"F0:18:98:00:00:01", "", false, -1, "COMPUTER", "Apple, Inc.", false},
		{"D4:11:22:33:44:55", "", true, 0x0087, "WEARABLE", "", false}, // Garmin by company category
	}
	for _, c := range cases {
		r := Classify(c.mac, c.name, c.ble, c.mfgr)
		if r.Category != c.code || r.Notable != c.notable || (c.maker != "" && r.Maker != c.maker) {
			t.Errorf("%s %q ble=%v mfgr=%d: got %+v, want %s %q notable=%v", c.mac, c.name, c.ble, c.mfgr, r, c.code, c.maker, c.notable)
		}
	}
}

func TestNoFalseHits(t *testing.T) {
	for _, c := range []struct {
		name string
		ble  bool
	}{
		{"Saxony Court", false},  // not Axon
		{"Firewall-5G", false},   // fire only counts on a Cradlepoint, and firewall is not fire
		{"flamingo", false},      // a toy name, but only on Bluetooth
		{"Flockhart Family", false},
		{"pineapple express", false},
	} {
		r := Classify("DA:A1:19:00:00:01", c.name, c.ble, -1)
		if r.Notable {
			t.Errorf("%q ble=%v: %+v", c.name, c.ble, r)
		}
	}
	// a MAC in an IEEE shared block gets the block's owner, not the registration authority
	if n := OUIName("70:B3:D5:71:F0:01"); n == "" || n == oui["70B3D5"] {
		t.Errorf("MA-S block: %q (MA-L %q)", n, oui["70B3D5"])
	}
	// a randomised address has no IEEE owner
	if r := Classify("F2:18:98:00:00:01", "", false, -1); r.Maker != "" || !r.Random {
		t.Errorf("random: %+v", r)
	}
}

func TestTokens(t *testing.T) {
	got := tokens("unit12 sheriff-cdf_ocfa7")
	want := []string{"unit", "12", "sheriff", "cdf", "ocfa", "7"}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
}
