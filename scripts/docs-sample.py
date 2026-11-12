"""Generate fictional Vancouver-area WiGLE logs for documentation screenshots.

Usage: python scripts/docs-sample.py --out build/docs-capture/logs
No real observations, network identities or device locations are used.
"""
import argparse
import csv
from datetime import datetime, timedelta
import math
from pathlib import Path
import random


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    rng = random.Random(32026)
    # An inland downtown grid, with a wide buffer from both shorelines.
    # Sample each direction so fits are supported by intersecting passes,
    # rather than a single shoreline trajectory with ambiguous lateral fits.
    def position(column, row):
        return (49.2802 + column*.0014 + row*.0009,
                -123.1245 + column*.00215 - row*.0014)

    vertices = []
    for row in range(3):
        vertices.extend((column, row) for column in
                        (range(4) if row % 2 == 0 else range(3, -1, -1)))
    # The horizontal traversal ends at (3, 2); continue along verticals.
    for column in range(3, -1, -1):
        vertices.extend((column, row) for row in
                        (range(2, -1, -1) if column % 2 else range(3)))
    route = [position(*vertex) for vertex in vertices]
    scale = 111195 * math.cos(math.radians(49.277))
    points = []
    for (lat, lon), (lat2, lon2) in zip(route, route[1:]):
        length = math.hypot((lat2-lat)*111195, (lon2-lon)*scale)
        for step in range(max(1, round(length/8))):
            f = step/max(1, round(length/8))
            points.append((lat+(lat2-lat)*f, lon+(lon2-lon)*f))
    aps = []
    for i in range(120):
        # Place fictional transmitters inside the six inland blocks. Do not
        # scatter them around every route point, which can put them in water.
        lat, lon = position(i % 3 + rng.uniform(.2, .8),
                            (i//3) % 2 + rng.uniform(.2, .8))
        aps.append((lat, lon,
                    f'02:CA:FE:{i >> 16:02X}:{i >> 8 & 255:02X}:{i & 255:02X}',
                    f'DEMO-{["Downtown", "Robson", "Burrard", "Library"][i % 4]}-{i:03}',
                    [2412, 2437, 2462, 5180, 5745, 6115][i % 6]))
    fields = ['MAC', 'SSID', 'AuthMode', 'FirstSeen', 'Channel', 'Frequency',
              'RSSI', 'CurrentLatitude', 'CurrentLongitude', 'AltitudeMeters',
              'AccuracyMeters', 'Type', 'Node']
    for drive in range(2):
        name = ['Vancouver-demo-morning.csv', 'Vancouver-demo-repeat.csv'][drive]
        path = args.out/name
        if path.exists():
            raise FileExistsError(f'Refusing to overwrite {path}')
        start = datetime(2026, 10, 17+drive, 17, 0)
        rows = []
        for tick, (lat, lon) in enumerate(points):
            for alat, alon, mac, ssid, freq in aps:
                distance = max(3, math.hypot((alat-lat)*111195, (alon-lon)*scale))
                if distance > (145 if drive == 0 else 125) or rng.random() < .08:
                    continue
                signal = round(-32-22*math.log10(distance)+rng.gauss(0, 1.7)-drive*2)
                channel = (freq-2407)//5 if freq < 3000 else (freq-(5950 if freq > 5900 else 5000))//5
                rows.append([mac, ssid, '[WPA2-PSK-CCMP][ESS]',
                             (start+timedelta(seconds=tick*2)).isoformat(' '),
                             channel, freq, signal, f'{lat:.7f}', f'{lon:.7f}',
                             12, 4, 'WIFI', 'Demo receiver'])
        # Real duplicate rows exercise import deduplication and provenance.
        rows += rows[::40]
        with path.open('x', newline='', encoding='utf-8') as stream:
            stream.write('WigleWifi-1.6,appRelease=documentation-demo,model=Synthetic Vancouver,release=1,device=Demo\n')
            writer = csv.writer(stream)
            writer.writerow(fields)
            writer.writerows(rows)
        print(f'{path}: {len(rows):,} synthetic source rows')


if __name__ == '__main__':
    main()
