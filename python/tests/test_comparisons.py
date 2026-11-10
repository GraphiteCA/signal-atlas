"""Joint fits honor selected BSSID/bands; pinned date scopes survive updates."""
import json
import math
from pathlib import Path
import sys
import tempfile
import time
import unittest

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from estimate_ap import LocationModel
from selection_estimates import comparisons, delete_comparison, save_comparison
from serve_survey import Jobs, fitted_estimates
from survey_data import backup_database, connect, import_csv, radios, summary
from test_survey import write_csv, row


class ComparisonTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.database = self.root/'survey.sqlite3'
        rows = []
        # Same MAC reported on two bands, different transmit power and locations.
        for day, shift in [('2026-01-01', 0), ('2026-03-10', .004)]:
            for radio, base in enumerate((48.449+shift, 48.450+shift)):
                for i in range(8):
                    angle=i*math.pi/4
                    distance=40+i*4
                    lat=base+math.sin(angle)*distance/111195.08
                    lon=2.668+math.cos(angle)*distance/(111195.08*math.cos(math.radians(base)))
                    rows.append(dict(row(len(rows),frequency='2412' if radio==0 else '5180',lat=lat,lon=lon),
                                     RSSI=-30-radio*15-30*math.log10(distance),FirstSeen=day+' 12:00:00'))
        self.source=self.root/'data.csv'
        write_csv(self.source,rows);import_csv(self.source,self.database)

    def tearDown(self):self.temp.cleanup()

    def fit(self,params):
        job=Jobs(self.database);job.estimate(params)
        end=time.monotonic()+20
        while job.snapshot()['status']=='running' and time.monotonic()<end:time.sleep(.01)
        self.assertEqual(job.snapshot()['status'],'complete',job.snapshot())
        return fitted_estimates(self.database,params)

    def test_optional_joint_fit_has_independent_band_intercepts_and_selection_identity(self):
        params={'date':['2026-01-01'],'shared':['1']}
        result=self.fit(params)
        self.assertEqual(len(result['estimates']),3)
        joint=next(e for e in result['estimates'] if e['co_location_assumed'])
        self.assertEqual(len(joint['members']),2)
        self.assertEqual(joint['used_observations'],16)
        self.assertEqual(set(joint['seen_bands']),{'2.4','5'})
        with connect(self.database) as db:
            raw=[dict(r,key=str(r['id']),mac=r['radio_key']) for r in db.execute("SELECT * FROM observations WHERE date='2026-01-01'")]
        model=LocationModel(raw)
        expected=model.fit(np.arange(len(raw)),3.,step_m=10,include_predictions=False)
        self.assertAlmostEqual(joint['latitude'],expected['latitude'],places=7)
        one=self.fit({**params,'band':['2.4']})
        self.assertEqual(len(one['estimates']),1)
        self.assertFalse(one['estimates'][0]['co_location_assumed'])
        self.assertGreater(abs(one['estimates'][0]['latitude']-joint['latitude']),.0001)
        self.assertEqual(fitted_estimates(self.database,params)['missing_estimates'],0)

    def test_candidate_lists_all_seen_bands_even_when_filtering_one(self):
        result=self.fit({'band':['5'],'date':['2026-01-01']})['estimates'][0]
        self.assertEqual(result['band'],'5')
        self.assertEqual(set(result['seen_bands']),{'2.4','5'})
        self.assertEqual(set(result['ssid_seen_bands']),{'2.4','5'})
        self.assertEqual(set(radios(self.database,{'band':['5']})['rows'][0]['seen_bands'].split(',')),{'2.4','5'})

    def test_missing_only_job_reuses_cached_fits(self):
        params={'date':['2026-01-01']}
        self.fit(params)
        job=Jobs(self.database);job.estimate({**params,'missing':['1']})
        while job.snapshot()['status']=='running':time.sleep(.01)
        self.assertEqual(job.snapshot()['result']['eligible_radios'],2)
        self.assertEqual(job.snapshot()['result']['attempted_fits'],0)

    def test_pinned_scopes_survive_other_fits_imports_and_backup(self):
        early={'date':['2026-01-01'],'band':['5']}
        late={'date':['2026-03-10'],'band':['5']}
        with self.assertRaisesRegex(ValueError,'finish'):save_comparison(self.database,early,'Before',fitted_estimates)
        self.fit(early);save_comparison(self.database,early,'Before',fitted_estimates)
        self.fit(late);save_comparison(self.database,late,'After',fitted_estimates)
        pinned=comparisons(self.database)['comparisons']
        self.assertEqual(len(pinned),2)
        self.assertAlmostEqual(pinned[1]['estimates'][0]['latitude']-pinned[0]['estimates'][0]['latitude'],.004,places=4)
        self.assertEqual(pinned[0]['observation_scope']['dates'],['2026-01-01'])
        write_csv(self.source,[row(99,ssid='New')]);import_csv(self.source,self.database)
        self.assertEqual(fitted_estimates(self.database,early)['estimates'],[])
        self.assertEqual(comparisons(self.database)['comparisons'],pinned)
        snapshot=backup_database(self.database,self.root/'backup.sqlite3')
        self.assertEqual(comparisons(snapshot)['comparisons'],pinned)
        delete_comparison(self.database,pinned[0]['id'])
        self.assertEqual(len(comparisons(self.database)['comparisons']),1)

    def test_ssid_search_is_a_literal_map_and_fit_filter(self):
        self.assertEqual(summary(self.database,{'ssid_query':['Shared']})['count'],32)
        self.assertEqual(summary(self.database,{'ssid_query':['%']})['count'],0)
        self.assertEqual(fitted_estimates(self.database,{'ssid_query':['nothing']})['missing_estimates'],0)
        self.assertEqual(summary(self.database,{'ssid_query':['']})['count'],32)

    def test_broad_shared_selection_keeps_map_and_independent_fits_available(self):
        write_csv(self.source,[row(100+i,mac=f'02:00:00:00:01:{i:02X}') for i in range(21)])
        import_csv(self.source,self.database)
        result=self.fit({'shared':['1']})
        self.assertEqual(summary(self.database)['count'],53)
        self.assertEqual(len(result['estimates']),2)
        self.assertTrue(result['shared_warning'])
        self.assertTrue(all(not e['co_location_assumed'] for e in result['estimates']))

    def test_comparison_names_and_layer_limit(self):
        params={'band':['5']}
        self.fit(params)
        with self.assertRaisesRegex(ValueError,'Name'):save_comparison(self.database,params,' ',fitted_estimates)
        for i in range(6):save_comparison(self.database,params,f'Layer {i+1}',fitted_estimates)
        with self.assertRaisesRegex(ValueError,'six'):save_comparison(self.database,params,'Seventh',fitted_estimates)
        self.assertEqual(len(comparisons(self.database)['comparisons']),6)

    def test_shared_only_excludes_individual_caches_and_jobs(self):
        self.fit({'shared':['1']})  # Seed both kinds, as older viewer sessions did.
        params={'shared':['1'],'individual':['0']}
        result=fitted_estimates(self.database,params)
        self.assertEqual(len(result['estimates']),1)
        self.assertTrue(result['estimates'][0]['co_location_assumed'])
        self.assertEqual(result['eligible_fits'],1)
        job=Jobs(self.database);job.estimate(params)
        while job.snapshot()['status']=='running':time.sleep(.01)
        self.assertEqual(job.snapshot()['result']['attempted_fits'],1)
        self.assertEqual(len(fitted_estimates(self.database,{'individual':['1']})['estimates']),2)
        save_comparison(self.database,params,'Shared only',fitted_estimates)
        self.assertEqual(len(comparisons(self.database)['comparisons'][0]['estimates']),1)

    def test_shared_only_single_radio_and_sparse_radios(self):
        params={'shared':['1'],'individual':['0'],'band':['5']}
        result=self.fit(params)
        self.assertEqual(len(result['estimates']),1)
        self.assertFalse(result['estimates'][0]['co_location_assumed'])
        # Each radio has only three observations, but together they provide
        # four within-radio contrasts and can fit a common AP location.
        with connect(self.database) as db:
            db.execute('DELETE FROM observations WHERE id NOT IN (SELECT id FROM observations WHERE band="5" LIMIT 3) AND id NOT IN (SELECT id FROM observations WHERE band="2.4" LIMIT 3)')
            db.execute('DELETE FROM estimates')
        result=self.fit({'shared':['1'],'individual':['0']})
        self.assertEqual(result['eligible_radios'],0)
        self.assertEqual(result['eligible_fits'],1)
        self.assertEqual(result['selected_radios'],2)
        self.assertEqual(result['estimates'][0]['used_observations'],6)
        self.assertEqual(result['estimates'][0]['status'],'fitted')


if __name__=='__main__':unittest.main()
