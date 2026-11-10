"""Estimate a fixed Wi-Fi transmitter location from GPS-tagged RSSI observations.

Model: RSSI_i = A_radio - 10 * n * log10(max(distance_i, floor_m)).
Each radio gets an independent fitted intercept. Combining radios assumes that
they are co-located; an SSID alone does not establish this. No calibrated
probabilities or confidence intervals are inferred.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path

import numpy as np
from survey_data import parse_observation, csv_reader, is_wifi

EARTH_RADIUS_M = 6_371_008.8
METRES_PER_DEGREE = math.pi * EARTH_RADIUS_M / 180


def read_observations(path: Path, ssid: str | None = None, mac: str | None = None) -> list[dict]:
    """Stream a selected network from ordinary CSV or a WiGLE export."""
    observations = []
    chosen = ssid
    with path.open(encoding="utf-8-sig", newline="") as handle:
        reader, first_line = csv_reader(handle)
        for line, row in enumerate(reader, first_line):
            if not is_wifi(row):
                continue
            if mac and (row.get("MAC") or "").strip().upper() != mac.strip().upper():
                continue
            if ssid is not None and row["SSID"] != ssid:
                continue
            if chosen is None:
                chosen = row["SSID"]
            elif row["SSID"] != chosen:
                raise ValueError("Select one network with --ssid, or use import_survey.py and serve_survey.py for multiple SSIDs")
            observations.append(parse_observation(row, line))
    if not observations:
        raise ValueError("No observations matched the selected network/radio")
    return observations


class LocalFrame:
    """Local equirectangular metre coordinates, for small survey footprints."""

    def __init__(self, latitude: float, longitude: float):
        self.latitude, self.longitude = latitude, longitude
        self.x_scale = METRES_PER_DEGREE * math.cos(math.radians(latitude))

    def to_xy(self, latitude, longitude):
        return np.column_stack(((np.asarray(longitude) - self.longitude) * self.x_scale,
                                (np.asarray(latitude) - self.latitude) * METRES_PER_DEGREE))

    def to_latlon(self, point):
        return [self.latitude + float(point[1]) / METRES_PER_DEGREE,
                self.longitude + float(point[0]) / self.x_scale]


class LocationModel:
    def __init__(self, observations: list[dict], distance_floor_m: float = 3,
                 weights=None, within_sse=None, cancelled=None):
        self.observations = observations
        if not observations:
            raise ValueError("No observations to fit")
        self.weights = np.ones(len(observations)) if weights is None else np.asarray(weights, dtype=float)
        self.within_sse = np.zeros(len(observations)) if within_sse is None else np.asarray(within_sse, dtype=float)
        if (self.weights.shape != (len(observations),) or self.within_sse.shape != (len(observations),)
                or not np.isfinite(self.weights).all() or not np.isfinite(self.within_sse).all()
                or (self.weights <= 0).any() or (self.within_sse < 0).any()):
            raise ValueError("Invalid observation weights or within-location residuals")
        self.cancelled = cancelled
        self.frame = LocalFrame(float(np.average([o["latitude"] for o in observations], weights=self.weights)),
                                float(np.average([o["longitude"] for o in observations], weights=self.weights)))
        self.xy = self.frame.to_xy([o["latitude"] for o in observations],
                                   [o["longitude"] for o in observations])
        if np.ptp(self.xy, axis=0).max() > 20_000:
            raise ValueError("Survey exceeds 20 km or crosses the dateline; split it into local areas")
        self.rssi = np.array([o["rssi_dbm"] for o in observations])
        self.macs = np.array([o["mac"] for o in observations])
        self.floor = distance_floor_m
        if len(np.unique(self.xy, axis=0)) < 3:
            raise ValueError("Need at least three distinct receiver positions")

    def contrast_count(self, indices):
        return int(self.weights[indices].sum()) - len(np.unique(self.macs[indices]))

    def sse(self, points, indices, exponent):
        """Profile out per-radio intercepts, evaluating candidate positions in chunks."""
        output = np.empty(len(points))
        groups = [self.macs[indices] == mac for mac in np.unique(self.macs[indices])]
        chunk = max(1, min(2048, 250_000 // max(1, len(indices))))
        for start in range(0, len(points), chunk):
            if self.cancelled and self.cancelled():
                raise InterruptedError("Estimation cancelled")
            p = points[start:start + chunk]
            distances = np.maximum(np.linalg.norm(p[:, None, :] - self.xy[indices], axis=2), self.floor)
            implied_intercepts = self.rssi[indices] + 10 * exponent * np.log10(distances)
            residual = implied_intercepts.copy()
            for group in groups:
                residual[:, group] -= np.average(implied_intercepts[:, group], axis=1, weights=self.weights[indices][group])[:,None]
            output[start:start + len(p)] = (np.square(residual)*self.weights[indices]).sum(axis=1)+self.within_sse[indices].sum()
        return output

    def fit(self, indices, exponent: float, extent_m: float = 500, step_m: float = 5,
            include_predictions=True) -> dict:
        indices = np.asarray(indices, dtype=int)
        if self.contrast_count(indices) < 3:
            raise ValueError("Need at least three within-radio contrasts (observations minus radios)")
        axis = np.linspace(-extent_m, extent_m, math.ceil(2 * extent_m / step_m) + 1)
        xx, yy = np.meshgrid(axis, axis)
        points = np.column_stack((xx.ravel(), yy.ravel()))
        errors = self.sse(points, indices, exponent)
        starts = []
        for j in np.argsort(errors):
            if all(np.linalg.norm(points[j] - prior) > 3 * step_m for prior in starts):
                starts.append(points[j])
            if len(starts) == 8:
                break
        candidates = []
        refine_step = min(.25, step_m / 4)
        for start in starts:
            axes = [np.arange(max(-extent_m, v - 1.2 * step_m),
                              min(extent_m, v + 1.2 * step_m) + refine_step / 2,
                              refine_step) for v in start]
            xx, yy = np.meshgrid(*axes)
            fine = np.column_stack((xx.ravel(), yy.ravel()))
            fine = np.clip(fine, -extent_m, extent_m)
            scores = self.sse(fine, indices, exponent)
            j = int(np.argmin(scores))
            candidates.append((float(scores[j]), fine[j]))
        error, point = min(candidates, key=lambda c: c[0])
        distance = np.maximum(np.linalg.norm(self.xy[indices] - point, axis=1), self.floor)
        intercepts = {}
        prediction = np.empty(len(indices))
        for mac in np.unique(self.macs[indices]):
            group = self.macs[indices] == mac
            intercept = float(np.average(self.rssi[indices][group] + 10 * exponent * np.log10(distance[group]), weights=self.weights[indices][group]))
            intercepts[str(mac)] = intercept
            prediction[group] = intercept - 10 * exponent * np.log10(distance[group])
        informative = int(sum(self.weights[indices][self.macs[indices] == mac].sum()
                          for mac in np.unique(self.macs[indices])
                          if self.weights[indices][self.macs[indices] == mac].sum() > 1))
        return {
            "latitude": self.frame.to_latlon(point)[0], "longitude": self.frame.to_latlon(point)[1],
            "east_m": float(point[0]), "north_m": float(point[1]), "path_loss_exponent": exponent,
            "sse_db2": error, "residual_rms_db": math.sqrt(error / informative),
            "radio_intercepts_dbm_at_1m": intercepts,
            "boundary_limited": bool(np.max(np.abs(point)) >= extent_m - step_m),
            "observation_count": int(self.weights[indices].sum()), "informative_observation_count": informative,
            "predictions": [{"key": self.observations[i]["key"], "distance_m": float(d),
                             "predicted_rssi_dbm": float(p), "residual_db": float(self.rssi[i] - p)}
                            for i, d, p in zip(indices, distance, prediction)] if include_predictions else [],
        }


def estimate(observations, exponent=3.0, extent_m=500.0, step_m=5.0,
             distance_floor_m=3.0, radius_m=30.0, sensitivity=True):
    if not all(math.isfinite(v) and v > 0 for v in (exponent, extent_m, step_m, distance_floor_m, radius_m)):
        raise ValueError("Exponent, grid dimensions, distance floor and radius must be positive and finite")
    if len(observations) > 5000:
        raise ValueError("Use import_survey.py and serve_survey.py for more than 5,000 observations; the explorer uses bounded per-radio fitting")
    if math.ceil(2 * extent_m / step_m) > 1000:
        raise ValueError("Grid exceeds 1,002,001 candidates; increase --grid-step-m")
    model = LocationModel(observations, distance_floor_m)
    indices = np.arange(len(observations))
    primary = model.fit(indices, exponent, extent_m, step_m)
    point = np.array([primary["east_m"], primary["north_m"]])
    trials, omissions, radio_fits = [], [], []
    if sensitivity:
        for n in sorted({2., 2.5, 3., 3.5, 4., exponent}):
            fit = primary if n == exponent else model.fit(indices, n, extent_m, step_m)
            trials.append({k: v for k, v in fit.items() if k not in ("predictions", "radio_intercepts_dbm_at_1m")})
        for i in indices:
            keep = indices[indices != i]
            if model.contrast_count(keep) < 3:
                continue
            fit = model.fit(keep, exponent, extent_m, step_m)
            omissions.append({"omitted_key": observations[i]["key"], "omitted_id": observations[i]["id"],
                              "latitude": fit["latitude"], "longitude": fit["longitude"],
                              "shift_m": float(np.linalg.norm([fit["east_m"] - point[0], fit["north_m"] - point[1]])),
                              "boundary_limited": fit["boundary_limited"]})
        for mac in sorted(set(model.macs)):
            keep = np.flatnonzero(model.macs == mac)
            if model.contrast_count(keep) >= 3:
                fit = model.fit(keep, exponent, extent_m, step_m)
                radio_fits.append({"mac": str(mac), **{k: v for k, v in fit.items()
                                                       if k not in ("predictions", "radio_intercepts_dbm_at_1m")}})
    weights = 10 ** ((model.rssi - model.rssi.max()) / 10)
    centroid = model.frame.to_latlon(np.average(model.xy, weights=weights, axis=0))
    # Descriptive surface, deliberately not normalized into a probability density.
    axis = np.linspace(-200, 200, 41)
    xx, yy = np.meshgrid(point[0] + axis, point[1] + axis)
    landscape_points = np.column_stack((xx.ravel(), yy.ravel()))
    rms = np.sqrt(model.sse(landscape_points, indices, exponent) / primary["informative_observation_count"])
    warnings = [
        "The search radius is a user-chosen planning radius, not a confidence interval.",
        "RSSI-to-distance parameters are uncalibrated; walls, antenna patterns and changes between surveys can bias the fit.",
        "GPS accuracy describes receiver positions, not access point location uncertainty.",
        "Measurements are 2D; receiver altitudes do not establish transmitter height.",
    ]
    if len(set(model.macs)) > 1:
        warnings.insert(0, "Combined fit assumes all selected MAC addresses are stationary and co-located; a shared SSID does not prove this.")
    if len({o["date"] for o in observations}) > 1:
        warnings.append("Survey dates differ; constant transmitter position and per-radio power are assumptions.")
    if primary["boundary_limited"]:
        warnings.append("Best fit is near the search boundary: enlarge --extent-m before interpreting it.")
    if len(observations) > 2:
        eigen = np.linalg.eigvalsh(np.cov(model.xy.T))
        if eigen[0] <= 0 or eigen[-1] / max(eigen[0], 1e-9) > 20:
            warnings.append("Receiver positions have weak cross-track geometry; alternative locations may fit similarly.")
    singletons = [str(mac) for mac in sorted(set(model.macs)) if sum(model.macs == mac) == 1]
    if singletons:
        warnings.append("Single-observation radios provide no position information with free intercepts: " + ", ".join(singletons))
    report = {
        "schema_version": 1, "ssid": observations[0]["ssid"], "observations": observations,
        "estimate": primary, "search_radius_m": radius_m,
        "method": "Profile least squares of log-distance RSSI, with one intercept per MAC",
        "settings": {"path_loss_exponent": exponent, "distance_floor_m": distance_floor_m,
                     "extent_m": extent_m, "coarse_grid_step_m": step_m, "refined_grid_step_m": min(.25, step_m / 4),
                     "origin_latitude": model.frame.latitude, "origin_longitude": model.frame.longitude},
        "sensitivity": {"path_loss_exponents": trials, "leave_one_out": omissions, "individual_radios": radio_fits},
        "weighted_receiver_centroid": {"latitude": centroid[0], "longitude": centroid[1],
                                       "note": "Signal-power-weighted receiver centroid; not a triangulated AP position"},
        "landscape": {"width": 41, "height": 41,
                      "southwest": model.frame.to_latlon(point - 200),
                      "northeast": model.frame.to_latlon(point + 200),
                      "rms_db_south_to_north": np.round(rms.reshape(41, 41), 3).tolist()},
        "warnings": warnings,
    }
    return report


def geodesic_circle(latitude, longitude, radius_m, vertices=128):
    """Return a closed WGS84-style lon/lat ring using spherical destinations."""
    lat, lon, delta = math.radians(latitude), math.radians(longitude), radius_m / EARTH_RADIUS_M
    ring = []
    for bearing in np.linspace(0, 2 * math.pi, vertices, endpoint=False):
        phi = math.asin(math.sin(lat) * math.cos(delta) + math.cos(lat) * math.sin(delta) * math.cos(bearing))
        lam = lon + math.atan2(math.sin(bearing) * math.sin(delta) * math.cos(lat),
                              math.cos(delta) - math.sin(lat) * math.sin(phi))
        ring.append([((math.degrees(lam) + 180) % 360) - 180, math.degrees(phi)])
    return ring + [ring[0]]


def to_geojson(report):
    e = report["estimate"]
    features = [{"type": "Feature", "geometry": {"type": "Point", "coordinates": [o["longitude"], o["latitude"]]},
                 "properties": {"kind": "observation", **o}} for o in report["observations"]]
    features.extend([
        {"type": "Feature", "geometry": {"type": "Point", "coordinates": [e["longitude"], e["latitude"]]},
         "properties": {"kind": "estimated_ap", "ssid": report["ssid"], "model": report["method"],
                        "path_loss_exponent": e["path_loss_exponent"],
                        "co_location_assumed": len({o["mac"] for o in report["observations"]}) > 1}},
        {"type": "Feature", "geometry": {"type": "Polygon", "coordinates": [geodesic_circle(e["latitude"], e["longitude"], report["search_radius_m"])]},
         "properties": {"kind": "search_area", "radius_m": report["search_radius_m"], "confidence_interval": False}},
    ])
    return {"type": "FeatureCollection", "features": features}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("csv", type=Path, help="GPS-tagged Wi-Fi CSV")
    parser.add_argument("--ssid", help="Exact SSID; inferred when the CSV contains one network")
    parser.add_argument("--mac", help="Use just this MAC, instead of assuming co-location")
    parser.add_argument("--path-loss-exponent", type=float, default=3.)
    parser.add_argument("--extent-m", type=float, default=500., help="Half-width of the grid about the mean receiver position")
    parser.add_argument("--grid-step-m", type=float, default=5.)
    parser.add_argument("--distance-floor-m", type=float, default=3.)
    parser.add_argument("--radius-m", type=float, default=30., help="Planning radius; not inferred uncertainty")
    parser.add_argument("--no-sensitivity", action="store_true", help="Skip alternate exponents, per-radio and leave-one-out fits")
    parser.add_argument("--output", type=Path, default=Path("outputs/ap_estimate.json"))
    args = parser.parse_args()
    try:
        observations = read_observations(args.csv, args.ssid, args.mac)
        report = estimate(observations, args.path_loss_exponent, args.extent_m, args.grid_step_m,
                          args.distance_floor_m, args.radius_m, not args.no_sensitivity)
        report["input"] = {"filename": args.csv.name, "sha256": hashlib.sha256(args.csv.read_bytes()).hexdigest()}
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2, allow_nan=False) + "\n", encoding="utf-8")
        geo = args.output.with_suffix(".geojson")
        geo.write_text(json.dumps(to_geojson(report), indent=2, allow_nan=False) + "\n", encoding="utf-8")
        e = report["estimate"]
        print(f"{report['ssid']}: {e['latitude']:.7f}, {e['longitude']:.7f}")
        print(f"{len(observations)} observations | {len(set(o['mac'] for o in observations))} radios | residual RMS {e['residual_rms_db']:.2f} dB")
        print(f"Planning radius {args.radius_m:g} m (not a confidence interval)")
        print(f"Saved {args.output} and {geo}")
        for warning in report["warnings"]:
            print("  - " + warning)
    except (OSError, ValueError) as exc:
        parser.exit(2, f"error: {exc}\n")


if __name__ == "__main__":
    main()
