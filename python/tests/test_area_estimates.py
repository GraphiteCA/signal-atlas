"""Viewport discovery, scoped caches and bounded background overview fits."""
import json
from pathlib import Path
import sys
import tempfile
import time
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from area_estimates import BATCH_SIZE, MAX_LOCATIONS, area_plan
from serve_survey import Jobs
from survey_data import connect, estimate_scope, import_csv, radios
from test_survey import row, write_csv


class AreaEstimateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.database = self.root / 'survey.sqlite3'
        self.params = {'individual': ['1'], 'zoom': ['17'], 'bbox': ['2.666,48.447,2.670,48.451']}

    def tearDown(self):
        self.temp.cleanup()

    def seed(self, rows):
        source = self.root / 'input.csv'
        write_csv(source, rows)
        import_csv(source, self.database)

    def cache(self, group, lat=48.449, lon=2.668, scope_params=None, **extra):
        _, scope_key = estimate_scope({'fit_mode': ['sample'], **(scope_params or {})})
        value = dict(group, status='fitted', latitude=lat, longitude=lon,
                     co_location_assumed=group.get('co_location_assumed', 'members' in group), **extra)
        with connect(self.database) as db:
            db.execute('INSERT OR REPLACE INTO estimates VALUES(?,?,?)',
                       (group['radio_key'], scope_key, json.dumps(value)))

    def test_default_overview_shows_saved_fits_and_plans_one_shared_ap(self):
        self.seed([row(i, frequency=freq) for freq in (2412, 5180) for i in range(6)])
        groups = radios(self.database, {})['rows']
        self.cache(groups[0])
        params = {k: v for k, v in self.params.items() if k != 'individual'}
        result, _, work = area_plan(self.database, params)
        self.assertEqual(result['estimates'], [])
        self.assertEqual(len(work), 1)
        self.assertEqual(len(work[0]['members']), 2)
        self.cache(work[0], lat=48.450, scope_params={'fit_mode': ['all']})
        result, _, work = area_plan(self.database, {**params, 'zoom': ['14']})
        self.assertEqual([e['latitude'] for e in result['estimates']], [48.450])
        self.assertEqual(work, [])

    def test_four_macs_with_missing_bands_and_stale_cached_models(self):
        self.seed([row(i, mac=f'02:00:00:00:00:{m:02X}', frequency=f)
                   for m in range(4) for f in ('', '5180' if m % 2 else '2412')
                   for i in range(6)])
        for band_group in radios(self.database, {})['rows']:
            self.cache(band_group)
        params = {**self.params, 'individual': ['0']}
        _, _, shared = area_plan(self.database, params)
        self.cache(shared[0])
        self.cache({**shared[0], 'radio_key': 'shared:old-subset',
                    'members': shared[0]['members'][:2]})
        result, _, work = area_plan(self.database, params)
        self.assertEqual([e['radio_key'] for e in result['estimates']], [shared[0]['radio_key']])
        self.assertEqual(work, [])
        result, _, radios_to_fit = area_plan(self.database, self.params)
        self.assertEqual(result['estimates'], [])
        self.assertEqual(len(radios_to_fit), 4)
        for group in radios_to_fit:
            self.assertEqual(group['count'], 12)
            self.assertFalse(group['co_location_assumed'])
            self.cache(group)
        result, _, _ = area_plan(self.database, self.params)
        self.assertEqual(len({e['mac'] for e in result['estimates']}), 4)
        self.assertEqual(len(result['estimates']), 4)

    def test_zoom_gate_and_invalid_bounds(self):
        # Zoomed-out requests do not even open the database.
        result, _, groups = area_plan(self.database, {'zoom': ['15']})
        self.assertTrue(result['zoom_required'])
        self.assertEqual(groups, [])
        self.assertFalse(self.database.exists())
        for bounds in ('nan,48,-123,49', '-123,48,-124,49', '-181,48,-123,49'):
            with self.assertRaises(ValueError):
                area_plan(self.database, {**self.params, 'bbox': [bounds]})
        with self.assertRaises(ValueError):
            area_plan(self.database, {'individual': ['1'], 'zoom': ['17']})

    def test_overview_ignores_checkbox_selection_and_never_assumes_colocation(self):
        self.seed([row(i, ssid='A', mac='02:00:00:00:00:01') for i in range(6)] +
                  [row(i, ssid='B', mac='02:00:00:00:00:02', frequency=5180) for i in range(6)])
        result, params, groups = area_plan(self.database, {**self.params, 'ssid': ['A'],
                    'exclude_ssid': ['B'], 'shared': ['1'], 'fit_mode': ['all']})
        self.assertEqual({g['ssid'] for g in groups}, {'A', 'B'})
        self.assertEqual(params['fit_mode'], ['sample'])
        self.assertNotIn('shared', params)
        result, _, groups = area_plan(self.database, {**self.params, 'band': ['5']})
        self.assertEqual([g['ssid'] for g in groups], ['B'])
        result, _, groups = area_plan(self.database, {**self.params, 'none': ['1']})
        self.assertEqual(groups, [])
        self.assertEqual(result['estimates'], [])

    def test_saved_display_and_background_discovery_match_full_plan(self):
        self.seed([row(i, ssid=name) for name in ('Saved', 'Missing') for i in range(6)])
        self.cache(radios(self.database, {'ssid': ['Saved']})['rows'][0])
        full, _, work = area_plan(self.database, self.params)
        display, _, display_work = area_plan(self.database, self.params, discover=False)
        plan, _, pending = area_plan(self.database, self.params, display=False)
        self.assertEqual([item['ssid'] for item in display['estimates']], ['Saved'])
        self.assertEqual(display['estimates'], full['estimates'])
        self.assertEqual(display_work, [])
        self.assertEqual(plan['estimates'], [])
        self.assertEqual([group['ssid'] for group in pending], ['Missing'])
        self.assertEqual(pending, work)

    def test_predicted_center_controls_cached_visibility_and_scope(self):
        self.seed([row(i, ssid='Inside', lat=48.47) for i in range(6)] +
                  [row(i, ssid='Outside', mac='02:00:00:00:00:02') for i in range(6)])
        groups = radios(self.database, {})['rows']
        for group in groups:
            self.cache(group, lat=48.449 if group['ssid'] == 'Inside' else 48.47)
        result, _, missing = area_plan(self.database, self.params)
        self.assertEqual([e['ssid'] for e in result['estimates']], ['Inside'])
        self.assertEqual(missing, [])
        # Detailed exact and other date scopes cannot substitute for an overview fit.
        result, _, _ = area_plan(self.database, {**self.params, 'date': ['2026-11-08']})
        self.assertEqual(result['estimates'], [])
        self.assertEqual(result['missing_estimates'], 1)
        result, _, _ = area_plan(self.database, {**self.params, 'ssid_query': ['Outside']})
        self.assertEqual(result['estimates'], [])

    def test_discovery_is_bounded_but_all_saved_locations_are_visible(self):
        self.seed([row(i, ssid=f'N{i:03}', mac=f'02:00:00:00:01:{i:02X}', lat=48.449, lon=2.668)
                   for i in range(100)])
        result, _, groups = area_plan(self.database, {**self.params, 'limit': ['999999']})
        self.assertEqual(len(groups), BATCH_SIZE)
        self.assertEqual(result['missing_estimates'], MAX_LOCATIONS)
        self.assertTrue(result['more'])
        for group in radios(self.database, {'limit': ['200']})['rows']:
            self.cache(group)
        result, _, groups = area_plan(self.database, self.params)
        self.assertEqual(len(result['estimates']), 100)
        self.assertEqual(groups, [])

    def test_background_fit_uses_full_scoped_history_and_caches_failures(self):
        self.seed([row(i) for i in range(6)] + [row(7, lat=48.46)] +
                  [row(1, ssid='Too few', mac='02:00:00:00:00:02')])
        _, params, groups = area_plan(self.database, self.params)
        jobs = Jobs(self.database)
        jobs.estimate(params, groups=groups, kind='area-estimate')
        deadline = time.monotonic() + 20
        while jobs.snapshot()['status'] == 'running' and time.monotonic() < deadline:
            time.sleep(.01)
        self.assertEqual(jobs.snapshot()['status'], 'complete', jobs.snapshot())
        self.assertEqual(jobs.snapshot()['kind'], 'area-estimate')
        result, _, missing = area_plan(self.database, self.params)
        self.assertEqual(missing, [])
        with connect(self.database) as db:
            saved = [json.loads(r[0]) for r in db.execute('SELECT result FROM estimates')]
        fit = next(e for e in saved if e['status'] == 'fitted')
        self.assertEqual(fit['count'], 7)  # Includes a receiver outside the viewport/halo.
        self.assertEqual(fit['fit_mode'], 'sample')
        self.assertFalse(fit['co_location_assumed'])
        self.assertEqual(sum(e['status'] == 'insufficient' for e in saved), 1)


if __name__ == '__main__':
    unittest.main()
