"""Deterministic synthetic survey for the parity tests: several SSIDs and radios, bands from
frequency or missing, hidden and Unicode SSIDs, quotes and commas, a WiGLE preamble, BLE rows,
invalid rows, two dates. Writes a WiGLE CSV."""
import csv, math, random, sys
from pathlib import Path

def rows(seed=11, n=4000):
    rnd = random.Random(seed)
    nets = [("Cafe, \"Corner\"", ["AA:00:00:00:00:01", "AA:00:00:00:00:02"], [2412, 5180]),
            ("", ["AA:00:00:00:00:10"], [2437]),
            ("Exampé \U0001F437", ["AA:00:00:00:00:20", "AA:00:00:00:00:21", "AA:00:00:00:00:22"], [None, 5745, None]),
            ("Mesh", ["AA:00:00:00:00:30", "AA:00:00:00:00:31"], [2462, "6.115 GHz"]),
            ("Lonely", ["AA:00:00:00:00:40"], [None]),
            ("Far_100%", ["AA:00:00:00:00:50"], [5500])]
    aps = {}
    for ssid, macs, freqs in nets:
        cx, cy = rnd.uniform(-300, 300), rnd.uniform(-300, 300)
        for mac, f in zip(macs, freqs):
            aps[mac] = (ssid, f, cx + rnd.uniform(-5, 5), cy + rnd.uniform(-5, 5), rnd.uniform(-35, -25))
    lat0, lon0 = 48.4449, 2.6640
    out = []
    for i in range(n):
        t = i / n
        x = 600 * math.cos(t * 7.1) * (0.4 + t) ; y = 500 * math.sin(t * 5.3) * (1.2 - t)
        lat = lat0 + y / 111195.08; lon = lon0 + x / (111195.08 * math.cos(math.radians(lat0)))
        mac = rnd.choice(list(aps))
        ssid, f, ax, ay, tx = aps[mac]
        d = max(1.0, math.hypot(x - ax, y - ay))
        rssi = round(tx - 30 * math.log10(d) + rnd.gauss(0, 3))
        day = "2026-01-13" if i < n * 0.6 else "2026-10-15"
        typ = "WIFI"
        if i % 97 == 0: typ = "BLE"
        r = {"MAC": mac.lower() if i % 13 == 0 else mac, "SSID": ssid, "AuthMode": "[WPA2-PSK-CCMP][ESS]", "FirstSeen": f"{day} 12:{i % 60:02d}:{(i * 7) % 60:02d}",
             "Channel": str((i % 11) + 1), "Frequency": "" if f is None else str(f), "RSSI": str(max(-149, min(0, rssi))),
             "CurrentLatitude": f"{lat:.8f}", "CurrentLongitude": f"{lon:.8f}", "AltitudeMeters": "20", "AccuracyMeters": "" if i % 5 else "4.5",
             "Type": typ}
        if i % 211 == 0: r["RSSI"] = "strong"           # invalid number
        if i % 307 == 0: r["CurrentLatitude"] = "91"    # out of range
        if i % 401 == 0: r["AccuracyMeters"] = "-1"     # invalid accuracy
        out.append(r)
    return out

FIELDS = ["MAC", "SSID", "AuthMode", "FirstSeen", "Channel", "Frequency", "RSSI", "CurrentLatitude", "CurrentLongitude", "AltitudeMeters", "AccuracyMeters", "Type"]

def write_csv(path, rs):
    with open(path, "w", newline="", encoding="utf-8") as f:
        f.write("WigleWifi-1.6,appRelease=parity,model=Test,release=1,device=Test\r\n")
        w = csv.DictWriter(f, FIELDS); w.writeheader()
        for i, r in enumerate(rs):
            w.writerow(r)
            if i % 500 == 0: f.write("\r\n")             # blank lines are skipped

if __name__ == "__main__":
    out = Path(sys.argv[1]); out.mkdir(parents=True, exist_ok=True)
    rs = rows()
    write_csv(out / "survey.csv", rs)
    print("ok")
