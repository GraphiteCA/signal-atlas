"""Create a portable interactive survey map and a dependency-free SVG overview."""
from __future__ import annotations

import argparse
import html
import json
import math
import re
from pathlib import Path
from survey_data import infer_band


def signal_color(rssi):
    """A consistent purple-to-cyan RSSI ramp, clamped to -95..-70 dBm."""
    t = max(0., min(1., (rssi + 95) / 25))
    a, b = (151, 126, 244), (70, 236, 202)
    return "#" + "".join(f"{round(x + t * (y-x)):02x}" for x, y in zip(a, b))


def make_svg(report):
    """Exact receiver positions in a north-up local metre plane; no invented geography."""
    e = report["estimate"]
    scale_x = 111195.08 * math.cos(math.radians(e["latitude"]))
    positions = [((o["longitude"]-e["longitude"])*scale_x,
                  (o["latitude"]-e["latitude"])*111195.08) for o in report["observations"]]
    radius = report["search_radius_m"]
    scope_note = ("Combined fit assumes co-located radios." if len({o["mac"] for o in report["observations"]}) > 1
                  else "Individual-radio fit; stable position assumed.")
    limit = max(radius * 1.25, max(math.hypot(*p) for p in positions) * 1.15, 100)
    unit = 270 / limit
    cx, cy = 380, 394
    def project(p):
        return cx + p[0] * unit, cy - p[1] * unit
    escape = html.escape
    marks = []
    for i, (o, p) in enumerate(zip(report["observations"], positions), 1):
        x, y = project(p)
        r = 5 + max(0, min(1, (o["rssi_dbm"] + 95) / 25)) * 5
        marks.append(f'<circle cx="{x:.2f}" cy="{y:.2f}" r="{r:.2f}" fill="{signal_color(o["rssi_dbm"])}" stroke="#0a1220" stroke-width="2"><title>#{i}: {escape(o["mac"])}; {o["rssi_dbm"]:g} dBm</title></circle>')
    grid = []
    spacing = 50 if limit < 400 else 100
    for tick in range(-int(limit//spacing)*spacing, int(limit//spacing)*spacing+1, spacing):
        a, b = project((tick, -limit)), project((tick, limit))
        c, d = project((-limit, tick)), project((limit, tick))
        grid += [f'<path d="M{a[0]:.1f},{a[1]:.1f} L{b[0]:.1f},{b[1]:.1f}"/>',
                 f'<path d="M{c[0]:.1f},{c[1]:.1f} L{d[0]:.1f},{d[1]:.1f}"/>']
    trial_marks = []
    for fit in report["sensitivity"]["path_loss_exponents"]:
        x, y = project(((fit["longitude"]-e["longitude"])*scale_x,
                        (fit["latitude"]-e["latitude"])*111195.08))
        trial_marks.append(f'<path d="M{x},{y-5} l5,5 -5,5 -5,-5 Z" fill="none" stroke="#f4bd70" stroke-width="1.5"/>')
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="1180" height="790" viewBox="0 0 1180 790" role="img" aria-label="{escape(report['ssid'])} receiver observations and estimated access point with a {radius:g}-metre planning radius">
<defs><radialGradient id="halo"><stop stop-color="#46ecca" stop-opacity=".16"/><stop offset="1" stop-color="#46ecca" stop-opacity=".025"/></radialGradient><linearGradient id="signal"><stop stop-color="#977ef4"/><stop offset="1" stop-color="#46ecca"/></linearGradient></defs>
<rect width="1180" height="790" fill="#080e19"/>
<g font-family="Arial, sans-serif" fill="#edf4fc">
<text x="42" y="42" fill="#46ecca" font-size="12" letter-spacing="3">WIFI LATERATION / SURVEY ATLAS</text>
<text x="42" y="88" font-size="34" font-weight="700">{escape(report['ssid'])}</text>
<text x="42" y="116" fill="#99abc3" font-size="14">Receiver observations + model-based transmitter estimate</text>
<g stroke="#1c2a3e" stroke-width="1" fill="none">{''.join(grid)}</g>
<circle cx="{cx}" cy="{cy}" r="{radius*unit:.2f}" fill="url(#halo)" stroke="#46ecca" stroke-dasharray="7 6"/>
<circle cx="{cx}" cy="{cy}" r="{radius*unit/2:.2f}" fill="none" stroke="#46ecca" stroke-opacity=".2"/>
{''.join(trial_marks)}{''.join(marks)}
<g stroke="#46ecca" stroke-width="2" fill="#080e19"><circle cx="{cx}" cy="{cy}" r="12"/><path d="M{cx-20},{cy}h11 M{cx+9},{cy}h11 M{cx},{cy-20}v11 M{cx},{cy+9}v11"/></g><circle cx="{cx}" cy="{cy}" r="4" fill="#46ecca"/>
<text x="{cx+20}" y="{cy-20}" fill="#46ecca" font-size="13">Fitted AP</text>
<text x="{cx}" y="{cy-radius*unit-12}" text-anchor="middle" fill="#46ecca" font-size="13">{radius:g} m search radius</text>
<text x="674" y="176" fill="#99abc3" font-size="12">N</text><path d="M679 190v38 m-5-30 5-8 5 8" stroke="#99abc3" fill="none"/>
<rect x="755" y="154" width="385" height="524" rx="18" fill="#101b2b" stroke="#24334b"/>
<text x="786" y="198" fill="#46ecca" font-size="12" letter-spacing="2">CANDIDATE LOCATION</text>
<text x="786" y="238" font-size="24">{e['latitude']:.6f}</text><text x="786" y="270" font-size="24">{e['longitude']:.6f}</text>
<text x="786" y="315" fill="#99abc3" font-size="14">{len(positions)} observations / {len(set(o['mac'] for o in report['observations']))} radios</text>
<text x="786" y="344" fill="#99abc3" font-size="14">Model residual RMS: {e['residual_rms_db']:.2f} dB</text>
<path d="M786 374H1108" stroke="#26364e"/>
<circle cx="792" cy="408" r="5" fill="#46ecca"/><text x="810" y="413" font-size="13">Receiver observations</text>
<path d="M792 434l5,5 -5,5 -5,-5 Z" fill="none" stroke="#f4bd70"/><text x="810" y="444" font-size="13">Path-loss sensitivity fits</text>
<rect x="786" y="468" width="320" height="6" rx="3" fill="url(#signal)"/>
<text x="786" y="498" fill="#99abc3" font-size="12">−95 dBm / weaker</text><text x="1106" y="498" text-anchor="end" fill="#99abc3" font-size="12">−70 dBm / stronger</text>
<text x="786" y="548" fill="#f4bd70" font-size="14">Planning area, not a confidence interval.</text>
<text x="786" y="577" fill="#99abc3" font-size="13">{scope_note}</text>
<text x="786" y="602" fill="#99abc3" font-size="13">n = {e['path_loss_exponent']:g}; free intercept per radio.</text>
<text x="786" y="627" fill="#99abc3" font-size="13">Position and power assumed stable over time.</text>
<path d="M56 687h{50*unit:.2f} m{-50*unit:.2f},-4v8 m{50*unit:.2f},-8v8" stroke="#c5d6e9" fill="none"/>
<text x="56" y="712" fill="#99abc3" font-size="12">50 metres</text>
<text x="42" y="755" fill="#99abc3" font-size="12">NORTH-UP LOCAL METRE PLANE · No basemap · Exact receiver coordinates, no positional jitter</text>
</g></svg>'''


def build(report_path: Path, output: Path, basemap_path: Path | None = None):
    report = json.loads(report_path.read_text(encoding="utf-8"))
    if report.get("schema_version") != 1 or not report.get("observations"):
        raise ValueError("Unsupported or empty estimate report")
    for observation in report["observations"]:
        if "band" not in observation:
            band, source, mhz = infer_band(observation.get("frequency"))
            observation.update(band=band, band_source=source, frequency_mhz=mhz)
    candidate = basemap_path or report_path.with_name("basemap.geojson")
    if candidate.exists():
        basemap = json.loads(candidate.read_text(encoding="utf-8"))
        bbox = basemap.get("metadata", {}).get("bbox", [])
        e = report["estimate"]
        if (basemap.get("type") != "FeatureCollection" or not basemap.get("features")
                or len(bbox) != 4 or not all(math.isfinite(v) for v in bbox)
                or not (bbox[0] <= e["longitude"] <= bbox[2] and bbox[1] <= e["latitude"] <= bbox[3])):
            raise ValueError("Local basemap must be a nonempty GeoJSON excerpt covering the fitted AP")
        report["basemap"] = basemap
    elif basemap_path is not None:
        raise ValueError(f"Local basemap does not exist: {basemap_path}")
    template = Path(__file__).with_name("templates").joinpath("survey_map.html").read_text(encoding="utf-8")
    # JSON lives in a non-executable script element; prevent HTML/script breakouts.
    payload = json.dumps(report, ensure_ascii=True, allow_nan=False).replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026")
    if template.count("__REPORT_JSON__") != 1:
        raise ValueError("Template must contain one report placeholder")
    local_renderer = Path(__file__).with_name("templates").joinpath("local_basemap.js").read_text(encoding="utf-8")
    satellite = Path(__file__).with_name("templates").joinpath("satellite.js").read_text(encoding="utf-8")
    satellite += "\n" + Path(__file__).with_name("templates").joinpath("report_satellite.js").read_text(encoding="utf-8")
    replacements = {"PAGE_TITLE": html.escape(report["ssid"]), "REPORT_JSON": payload, "BASEMAP_JS": local_renderer,
                    "SATELLITE_JS": satellite}
    document = re.sub(r"__(PAGE_TITLE|REPORT_JSON|BASEMAP_JS|SATELLITE_JS)__", lambda match: replacements[match.group(1)], template)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(document, encoding="utf-8")
    svg = output.with_suffix(".svg")
    svg.write_text(make_svg(report), encoding="utf-8")
    return output, svg


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path, nargs="?", default=Path("outputs/ap_estimate.json"))
    parser.add_argument("--output", type=Path, default=Path("outputs/demo-map.html"))
    parser.add_argument("--basemap", type=Path, help="Local vector GeoJSON; defaults to basemap.geojson beside the report")
    args = parser.parse_args()
    try:
        for path in build(args.report, args.output, args.basemap):
            print(f"Saved {path.resolve()}")
    except (OSError, ValueError, KeyError) as exc:
        parser.exit(2, f"error: {exc}\n")


if __name__ == "__main__":
    main()
