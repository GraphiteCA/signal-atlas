"""Synthetic WiGLE drives for the compare parity test: a shared road driven on several days by two
rigs, one with a Node column (cluster logs), gaps that split legs, BLE rows, quoted SSIDs, corrupt
and GPS-less rows, an out-of-order row, two files that run on from each other."""
import datetime
import math
import random
import sys
from pathlib import Path

HDR = "MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,Type"


def aps(rnd, n=900):
    out = []
    for i in range(n):
        d = rnd.uniform(0, 16000)
        ch = rnd.choice([1, 6, 11, 36, 44, 149, 157, 5])
        fq = 2407 + ch * 5 if ch < 20 else 5000 + ch * 5
        if i % 50 == 0:
            fq = 6135
        mac = "%02X:%02X:%02X:%02X:%02X:%02X" % ((rnd.randrange(64) * 4) | (2 if i % 9 == 0 else 0), rnd.randrange(256), rnd.randrange(256), i >> 8, i & 255, rnd.randrange(256))
        if i % 7 == 0:
            ssid = ""
        elif i % 11 == 0:
            ssid = 'Café "%d", ok' % i
        else:
            ssid = "net-%d" % (i // 2)
        if i % 13 == 0:
            auth = "OPEN"
        elif i % 5 == 0:
            auth = "[WPA3-SAE-CCMP][ESS]"
        elif i % 31 == 0:
            auth = "[WEP][ESS]"
        else:
            auth = "[WPA2-PSK-CCMP][ESS]"
        out.append(dict(d=d, off=rnd.uniform(10, 120), tx=rnd.uniform(-45, -30), mac=mac, ssid=ssid, auth=auth, ch=ch, fq=fq))
    return out


def road(d):
    return 41.88 + d / 110950, -87.63 + math.sin(d / 2500) * .012


def q(s):
    return '"' + s.replace('"', '""') + '"' if any(c in s for c in ',"') else s


def drive(rnd, ap, start, p_see, rig, node=False, d0=0, d1=16000, gap_at=None, ble=False):
    lines = ["WigleWifi-1.6,appRelease=test,model=%s,release=1,device=%s" % (rig, rig), HDR + (",Node" if node else "")]
    t, d = start, d0
    step = 1 if d1 > d0 else -1
    gapped = False
    while (d < d1) if step > 0 else (d > d1):
        v = 8 + 7 * abs(math.sin(d / 1800))
        if int(t) % 97 == 0:
            v = 0.5                              # stops
        d += step * v
        t += 1
        if gap_at is not None and not gapped and ((step > 0 and d >= gap_at) or (step < 0 and d <= gap_at)):
            t += 1500                            # a gap: a new leg
            gapped = True
        la, lo = road(d)
        stamp = datetime.datetime.fromtimestamp(t, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
        for a in ap:
            if abs(a["d"] - d) > 300 or rnd.random() > p_see:
                continue
            dist = math.hypot(a["d"] - d, a["off"])
            rs = round(a["tx"] - 25 * math.log10(max(1, dist)) + rnd.gauss(0, 3))
            if rs < -95:
                continue
            row = [a["mac"], q(a["ssid"]), a["auth"], stamp, str(a["ch"]), str(a["fq"]), str(rs), "%.7f" % la, "%.7f" % lo, "20", "3", "WIFI"]
            if node:
                row.append("node-%d" % (a["ch"] % 3))
            lines.append(",".join(row))
        if ble and int(t) % 4 == 0:
            row = ["C4:%02X:00:00:00:%02X" % (int(t) % 50, int(t) % 7), "", "Misc", stamp, "0", "0", str(-60 - int(t) % 30), "%.7f" % la, "%.7f" % lo, "20", "3", "BLE"]
            if node:
                row.append("node-1")
            lines.append(",".join(row))
        if int(t) % 211 == 0:
            lines.append("not,a,row")
        if int(t) % 307 == 0:
            lines.append(",".join([ap[0]["mac"], "x", "OPEN", stamp, "1", "2412", "-70", "0", "0", "0", "0", "WIFI"] + (["node-0"] if node else [])))
    return lines


def main(out):
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    rnd = random.Random(5)
    ap = aps(rnd)
    t0 = 1767261600
    files = {
        "a-mon.csv": drive(rnd, ap, t0, .35, "Rig-A", gap_at=7000),
        "a-tue.csv": drive(rnd, ap, t0 + 86400, .3, "Rig-A", d0=16000, d1=0, ble=True),
        "b-wed.csv": drive(rnd, ap, t0 + 2 * 86400 + 3600, .4, "Rig-B", node=True, d0=3000, d1=12000, ble=True),
        "b-wed-2.csv": drive(rnd, ap, t0 + 2 * 86400 + 3600 + 720, .4, "Rig-B", node=True, d0=12000, d1=15000),
    }
    files["a-mon.csv"].insert(40, files["a-mon.csv"][60])   # an out-of-order row
    for name, lines in files.items():
        (out / name).write_bytes(("\r\n".join(lines) + "\r\n").encode("utf-8"))
    print("ok")


main(sys.argv[1])
