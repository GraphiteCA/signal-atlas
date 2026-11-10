"""Download one small, reusable OSM vector excerpt, never raster map tiles.

Uses the public Overpass data API with a bounded query and no automatic retries.
Existing output is reused unless --refresh is explicitly supplied. Rendering the
map subsequently needs no tile server, API key, or referrer header.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import urllib.parse
import urllib.request

OVERPASS_URL = "https://overpass-api.de/api/interpreter"


def query_for(latitude, longitude, half_width_m):
    if not all(math.isfinite(v) for v in (latitude, longitude, half_width_m)):
        raise ValueError("Coordinates and excerpt width must be finite")
    if not (-85 < latitude < 85 and -180 < longitude < 180 and 100 <= half_width_m <= 500):
        raise ValueError("Use a latitude within ±85°, and an excerpt half-width between 100 and 500 m")
    dy = half_width_m / 111195.08
    dx = dy / math.cos(math.radians(latitude))
    bbox = [longitude - dx, latitude - dy, longitude + dx, latitude + dy]
    if bbox[0] < -180 or bbox[2] > 180:
        raise ValueError("Dateline-crossing excerpts are not supported")
    south, west, north, east = bbox[1], bbox[0], bbox[3], bbox[2]
    area = f"({south:.7f},{west:.7f},{north:.7f},{east:.7f})"
    selectors = ['["highway"]', '["building"]', '["leisure"]', '["landuse"]',
                 '["natural"~"^(water|wood|scrub)$"]', '["waterway"]']
    query = '[out:json][timeout:25];(' + ''.join('way' + s + area + ';' for s in selectors) + ');out tags geom;'
    return bbox, query


def convert_response(raw, bbox, query):
    if raw.get("remark"):
        raise ValueError("Overpass did not return a complete excerpt: " + raw["remark"])
    if not isinstance(raw.get("elements"), list):
        raise ValueError("Invalid Overpass response")
    features = []
    for element in raw["elements"]:
        if element.get("type") != "way":
            continue
        geometry = element.get("geometry", [])
        # Never bridge missing segments or invent a closed footprint.
        if len(geometry) < 2 or any("lat" not in p or "lon" not in p for p in geometry):
            continue
        coords = [[float(p["lon"]), float(p["lat"])] for p in geometry]
        if not all(math.isfinite(v) for p in coords for v in p):
            raise ValueError("Non-finite map coordinate")
        tags = element.get("tags", {})
        closed = len(coords) >= 4 and coords[0] == coords[-1]
        area = closed and (any(k in tags for k in ("building", "landuse", "leisure", "natural"))
                           or tags.get("area") == "yes")
        # Minimal display tags; the raw response contains the original way tags.
        keep = {k: tags[k] for k in ("name", "highway", "building", "leisure", "landuse", "natural", "waterway", "addr:housenumber") if k in tags}
        features.append({"type": "Feature", "id": f"way/{element['id']}", "properties": keep,
                         "geometry": {"type": "Polygon" if area else "LineString",
                                      "coordinates": [coords] if area else coords}})
    if not features:
        raise ValueError("No displayable geometry returned; keep the survey grid instead")
    return {
        "type": "FeatureCollection", "features": features,
        "metadata": {"bbox": bbox, "source": "OpenStreetMap via Overpass API", "source_url": OVERPASS_URL,
                     "attribution": "© OpenStreetMap contributors", "license": "ODbL-1.0",
                     "license_url": "https://opendatacommons.org/licenses/odbl/1-0/",
                     "copyright_url": "https://www.openstreetmap.org/copyright",
                     "downloaded_utc": datetime.now(timezone.utc).isoformat(),
                     "osm_base_timestamp": raw.get("osm3s", {}).get("timestamp_osm_base"),
                     "query": query,
                     "scope": "Small local excerpt; standalone tagged ways only, not a complete or navigable map database. Multipolygon relations are not assembled."},
    }


def download(report_path: Path, output: Path, half_width_m=400., refresh=False):
    report = json.loads(report_path.read_text(encoding="utf-8"))
    point = report["estimate"]
    bbox, query = query_for(point["latitude"], point["longitude"], half_width_m)
    if output.exists() and not refresh:
        cached = json.loads(output.read_text(encoding="utf-8"))
        saved = cached.get("metadata", {}).get("bbox", [])
        if len(saved) != 4 or not (saved[0] <= point["longitude"] <= saved[2] and saved[1] <= point["latitude"] <= saved[3]):
            raise ValueError("Cached excerpt does not cover this estimate; choose another --output or explicitly --refresh")
        print(f"Reusing {output}; no network request")
        return cached
    request = urllib.request.Request(OVERPASS_URL,
        data=urllib.parse.urlencode({"data": query}).encode("ascii"),
        headers={"User-Agent": "SignalAtlas/1.1 (local Wi-Fi survey map excerpt)",
                 "Accept": "application/json", "Content-Type": "application/x-www-form-urlencoded"})
    with urllib.request.urlopen(request, timeout=45) as response:
        content = response.read(20_000_001)
    if len(content) > 20_000_000:
        raise ValueError("Excerpt exceeds 20 MB; choose a smaller area")
    raw = json.loads(content)
    converted = convert_response(raw, bbox, query)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.with_suffix(".overpass.json").write_bytes(content)
    output.write_text(json.dumps(converted, separators=(",", ":"), ensure_ascii=False, allow_nan=False) + "\n", encoding="utf-8")
    print(f"Saved {len(converted['features'])} local vector features to {output}")
    return converted


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path, nargs="?", default=Path("outputs/ap_estimate.json"))
    parser.add_argument("--output", type=Path, default=Path("outputs/basemap.geojson"))
    parser.add_argument("--half-width-m", type=float, default=400.)
    parser.add_argument("--refresh", action="store_true", help="Explicitly replace the cached excerpt with one fresh request")
    args = parser.parse_args()
    try:
        download(args.report, args.output, args.half_width_m, args.refresh)
    except (OSError, ValueError, KeyError) as exc:
        parser.exit(2, f"error: {exc}\nNo retries were made; the offline survey grid remains available.\n")


if __name__ == "__main__":
    main()
