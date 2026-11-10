"""Behavioral checks for estimation, geographic output and safe report embedding."""
import csv
import json
import math
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from estimate_ap import EARTH_RADIUS_M, LocalFrame, LocationModel, estimate, geodesic_circle, read_observations, to_geojson
from plot_observations import build
from download_basemap import convert_response, download, query_for


def synthetic():
    frame = LocalFrame(48.4491, 2.668)
    transmitter = np.array([21., 34.])
    positions = [(-90, -70), (100, -50), (110, 90), (-70, 100), (15, -20), (60, 20)]
    rows = []
    for mac, intercept in [("AA:00:00:00:00:01", -38.), ("AA:00:00:00:00:02", -53.)]:
        for point in positions:
            lat, lon = frame.to_latlon(point)
            rows.append({"key": str(len(rows)), "id": str(len(rows)), "ssid": "Synthetic",
                         "mac": mac, "latitude": lat, "longitude": lon,
                         "rssi_dbm": intercept - 30 * math.log10(np.linalg.norm(np.array(point) - transmitter)),
                         "accuracy_m": 3., "date": "2026-01-01", "first_seen": "2026-01-01 12:00:00"})
    return rows, frame.to_latlon(transmitter)


class EstimatorTests(unittest.TestCase):
    def test_recovers_known_transmitter_with_different_radio_powers(self):
        rows, truth = synthetic()
        model = LocationModel(rows)
        fitted = model.fit(np.arange(len(rows)), 3., 200, 5)
        truth_xy = model.frame.to_xy([truth[0]], [truth[1]])[0]
        error = np.linalg.norm(truth_xy - [fitted["east_m"], fitted["north_m"]])
        self.assertLess(error, .5)
        self.assertLess(fitted["residual_rms_db"], .05)

    def test_radio_power_shift_does_not_move_location(self):
        rows, _ = synthetic()
        model = LocationModel(rows)
        before = model.fit(np.arange(len(rows)), 3., 200, 5)
        shifted = [dict(o, rssi_dbm=o["rssi_dbm"] + (12 if o["mac"].endswith("02") else 0)) for o in rows]
        after = LocationModel(shifted).fit(np.arange(len(rows)), 3., 200, 5)
        self.assertAlmostEqual(before["latitude"], after["latitude"], places=8)
        self.assertAlmostEqual(before["longitude"], after["longitude"], places=8)

    def test_search_circle_is_100_metres_and_closed(self):
        lat, lon = 48.4494, 2.6684
        ring = geodesic_circle(lat, lon, 100)
        self.assertEqual(ring[0], ring[-1])
        self.assertEqual(len(ring), 129)
        for longitude, latitude in ring:
            a, b = math.radians(latitude-lat), math.radians(longitude-lon)
            h = math.sin(a/2)**2 + math.cos(math.radians(lat))*math.cos(math.radians(latitude))*math.sin(b/2)**2
            distance = 2*EARTH_RADIUS_M*math.asin(math.sqrt(h))
            self.assertAlmostEqual(distance, 100, places=5)

    def test_search_circle_supports_five_metres(self):
        latitude,longitude=48.4494,2.6684
        first=geodesic_circle(latitude,longitude,5)[0]
        self.assertAlmostEqual((first[1]-latitude)*math.pi/180*6371008.8,5,places=6)

    def test_uninformative_radios_do_not_locate_a_transmitter(self):
        rows, _ = synthetic()
        for i, row in enumerate(rows):
            row["mac"] = str(i)
        with self.assertRaisesRegex(ValueError, "contrasts"):
            LocationModel(rows).fit(np.arange(len(rows)), 3., 200, 5)

    def test_invalid_numeric_csv_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"invalid.csv"
            path.write_text("MAC,SSID,RSSI,CurrentLatitude,CurrentLongitude\nA,Test,-70,nan,-123\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "Non-finite"):
                read_observations(path)

    def test_multiple_networks_require_selection(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"multiple.csv"
            path.write_text("MAC,SSID,RSSI,CurrentLatitude,CurrentLongitude\nA,One,-70,48,-123\nB,Two,-80,48,-123\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "--ssid"):
                read_observations(path)
            self.assertEqual(len(read_observations(path, ssid="One")), 1)

    def test_report_is_json_serializable_and_geojson_has_correct_coordinate_order(self):
        rows, _ = synthetic()
        report = estimate(rows, extent_m=200, sensitivity=False)
        json.dumps(report, allow_nan=False)
        geo = to_geojson(report)
        self.assertEqual(len(geo["features"]), len(rows)+2)
        self.assertEqual(geo["features"][0]["geometry"]["coordinates"], [rows[0]["longitude"], rows[0]["latitude"]])
        self.assertFalse(geo["features"][-1]["properties"]["confidence_interval"])

    def test_report_embedding_cannot_close_script_element(self):
        rows, _ = synthetic()
        report = estimate(rows, extent_m=200, sensitivity=False)
        report["ssid"] = '</script><script>alert("unsafe")</script>'
        with tempfile.TemporaryDirectory() as directory:
            source, output = Path(directory)/"report.json", Path(directory)/"map.html"
            source.write_text(json.dumps(report), encoding="utf-8")
            build(source, output)
            document = output.read_text(encoding="utf-8")
            self.assertNotIn(report["ssid"], document)
            data = document.split('<script id="survey-data" type="application/json">')[1].split('</script>')[0]
            self.assertEqual(json.loads(data)["ssid"], report["ssid"])

    def test_ssid_that_looks_like_template_tokens_is_preserved(self):
        rows, _ = synthetic()
        report = estimate(rows, extent_m=200, sensitivity=False)
        report["ssid"] = "__PAGE_TITLE____REPORT_JSON__"
        with tempfile.TemporaryDirectory() as directory:
            source, output = Path(directory)/"report.json", Path(directory)/"map.html"
            source.write_text(json.dumps(report), encoding="utf-8")
            build(source, output)
            data = output.read_text(encoding="utf-8").split('<script id="survey-data" type="application/json">')[1].split('</script>')[0]
            self.assertEqual(json.loads(data)["ssid"], report["ssid"])

    def test_local_geometry_keeps_coordinate_order_and_only_closes_real_areas(self):
        raw = {"elements": [
            {"type": "way", "id": 1, "tags": {"highway": "residential", "name": "Test road"},
             "geometry": [{"lat": 10, "lon": 20}, {"lat": 10.001, "lon": 20.001}]},
            {"type": "way", "id": 2, "tags": {"building": "yes"},
             "geometry": [{"lat": 10, "lon": 20}, {"lat": 10.001, "lon": 20},
                          {"lat": 10.001, "lon": 20.001}, {"lat": 10, "lon": 20}]},
            {"type": "way", "id": 3, "tags": {"building": "yes"},
             "geometry": [{"lat": 10, "lon": 20}, {"lat": 10.001, "lon": 20},
                          {"lat": 10.001, "lon": 20.001}]},
        ]}
        result = convert_response(raw, [19, 9, 21, 11], 'test fixture')
        self.assertEqual(result["features"][0]["geometry"]["coordinates"][0], [20., 10.])
        self.assertEqual(result["features"][1]["geometry"]["type"], "Polygon")
        self.assertEqual(result["features"][2]["geometry"]["type"], "LineString")
        self.assertEqual(result["metadata"]["license"], "ODbL-1.0")

    def test_cached_excerpt_does_not_make_a_network_request(self):
        with tempfile.TemporaryDirectory() as directory:
            source, output = Path(directory)/"report.json", Path(directory)/"basemap.geojson"
            source.write_text(json.dumps({"estimate": {"latitude": 10., "longitude": 20.}}), encoding="utf-8")
            output.write_text(json.dumps({"metadata": {"bbox": [19., 9., 21., 11.]}}), encoding="utf-8")
            with patch('download_basemap.urllib.request.urlopen') as request:
                download(source, output)
                request.assert_not_called()

    def test_data_query_is_small_and_contains_no_network_identifiers(self):
        bbox, query = query_for(10, 20, 400)
        self.assertLess((bbox[2]-bbox[0])*(bbox[3]-bbox[1]), .0001)
        self.assertIn('[timeout:25]', query)
        self.assertNotIn('MAC', query)
        self.assertNotIn('SSID', query)
        with self.assertRaisesRegex(ValueError, '500'):
            query_for(10, 20, 1000)

    def test_generated_report_never_requests_raster_tiles(self):
        rows, _ = synthetic()
        report = estimate(rows, extent_m=200, sensitivity=False)
        with tempfile.TemporaryDirectory() as directory:
            source, output = Path(directory)/"report.json", Path(directory)/"map.html"
            source.write_text(json.dumps(report), encoding="utf-8")
            build(source, output)
            document = output.read_text(encoding="utf-8")
            self.assertNotIn('tile.openstreetmap.org', document)
            self.assertIn('class SatelliteLayer', document)
            self.assertIn('Local streets not embedded', document)

    def test_basemap_outside_the_estimate_is_rejected(self):
        rows, _ = synthetic()
        report = estimate(rows, extent_m=200, sensitivity=False)
        with tempfile.TemporaryDirectory() as directory:
            source, output, basemap = Path(directory)/"report.json", Path(directory)/"map.html", Path(directory)/"basemap.geojson"
            source.write_text(json.dumps(report), encoding="utf-8")
            basemap.write_text(json.dumps({"type": "FeatureCollection", "features": [{}], "metadata": {"bbox": [19., 9., 21., 11.]}}), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "covering"):
                build(source, output, basemap)


if __name__ == "__main__":
    unittest.main()
