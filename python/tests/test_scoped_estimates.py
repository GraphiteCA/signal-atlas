"""Verify real date-scoped fitting and the all-observation objective."""
import csv
import json
import math
from pathlib import Path
import sys
import tempfile
import time
import unittest

import numpy as np

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from estimate_ap import LocationModel
from serve_survey import Jobs, fitted_estimates
from survey_data import (available_dates, connect, database_info, discover_csv_files,
                         estimate_scope, import_csv, initialize, radios, summary)

MAC='02:00:00:00:00:01'


def survey_row(i, day, base=48.449, repeat=False):
    angle=(i%6)*math.pi/3
    distance=45+(i%6)*3
    lat=base+math.sin(angle)*distance/111195.08
    lon=2.668+math.cos(angle)*distance/(111195.08*math.cos(math.radians(base)))
    return {'MAC':MAC,'SSID':'Moving AP','RSSI':-38-30*math.log10(distance)+((i//6)%3-1 if repeat else 0),
            'CurrentLatitude':lat,'CurrentLongitude':lon,'Frequency':5180,
            'FirstSeen':day+' 23:59:59' if day else ''}


class ScopedEstimateTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.root=Path(self.temp.name)
        self.database=self.root/'survey.sqlite3'

    def tearDown(self):self.temp.cleanup()

    def seed(self,rows):
        source=self.root/'data.csv'
        with source.open('w',newline='',encoding='utf-8') as handle:
            writer=csv.DictWriter(handle,fieldnames=rows[0].keys())
            writer.writeheader();writer.writerows(rows)
        import_csv(source,self.database)

    def fit(self,params):
        jobs=Jobs(self.database);jobs.estimate(params)
        deadline=time.monotonic()+20
        while jobs.snapshot()['status']=='running' and time.monotonic()<deadline:time.sleep(.01)
        self.assertEqual(jobs.snapshot()['status'],'complete',jobs.snapshot())
        return fitted_estimates(self.database,params)['estimates']

    def test_range_and_specific_dates_really_restrict_the_fit(self):
        self.seed([survey_row(i,'2026-01-01') for i in range(6)]+
                  [survey_row(i,'2026-03-10',base=48.453) for i in range(6)])
        early={'date_from':['2026-01-01'],'date_to':['2026-01-01']}
        first=self.fit(early)[0]
        self.assertEqual(first['used_observations'],6)
        self.assertAlmostEqual(first['latitude'],48.449,places=4)
        self.assertEqual(first['first_date'],'2026-01-01')
        late={'date':['2026-03-10']}
        self.assertEqual(fitted_estimates(self.database,late)['estimates'],[])
        last=self.fit(late)[0]
        self.assertEqual(last['used_observations'],6)
        self.assertAlmostEqual(last['latitude'],48.453,places=4)
        self.assertGreater(abs(last['latitude']-first['latitude']),.003)
        self.assertEqual(fitted_estimates(self.database,{})['estimates'],[])
        all_rows=self.fit({})[0]
        self.assertEqual(all_rows['used_observations'],12)
        self.assertEqual(len(fitted_estimates(self.database,early)['estimates']),1)
        with connect(self.database) as db:self.assertEqual(db.execute('SELECT count(*) FROM estimates').fetchone()[0],3)

    def test_all_mode_uses_more_than_256_measurements(self):
        self.seed([survey_row(i,'2026-01-01',repeat=True) for i in range(400)])
        exact=self.fit({})[0]
        self.assertEqual(exact['fit_mode'],'all')
        self.assertEqual(exact['used_observations'],400)
        self.assertEqual(exact['coordinate_groups'],6)
        self.assertIsNone(exact['sample_limit'])
        self.assertEqual(fitted_estimates(self.database,{'fit_mode':['sample']})['estimates'],[])
        quick=self.fit({'fit_mode':['sample']})[0]
        self.assertLessEqual(quick['used_observations'],256)
        self.assertEqual(fitted_estimates(self.database,{})['estimates'][0]['used_observations'],400)

    def test_weighted_coordinate_aggregation_matches_raw_objective(self):
        raw=[]
        for i in range(18):
            source=survey_row(i,'2026-01-01',repeat=True)
            raw.append({'key':str(i),'mac':MAC,'latitude':source['CurrentLatitude'],
                        'longitude':source['CurrentLongitude'],'rssi_dbm':source['RSSI']})
        grouped={}
        for row in raw:grouped.setdefault((row['latitude'],row['longitude']),[]).append(row)
        combined=[];weights=[];within=[]
        for members in grouped.values():
            values=np.array([r['rssi_dbm'] for r in members])
            combined.append(dict(members[0],rssi_dbm=float(values.mean())))
            weights.append(len(members));within.append(float(((values-values.mean())**2).sum()))
        full=LocationModel(raw);compressed=LocationModel(combined,weights=weights,within_sse=within)
        candidates=np.array([[0,0],[10,20],[-35,45],[100,-80]])
        np.testing.assert_allclose(full.sse(candidates,np.arange(len(raw)),3),
                                   compressed.sse(candidates,np.arange(len(combined)),3),rtol=1e-9,atol=1e-7)

    def test_signal_filter_restricts_observations_and_has_a_separate_cache(self):
        self.seed([survey_row(i,'2026-01-01',repeat=True) for i in range(400)])
        params={'min_rssi':['-90']}
        count=summary(self.database,params)['count']
        fitted=self.fit(params)[0]
        self.assertGreater(count,4)
        self.assertLess(count,400)
        self.assertEqual(fitted['used_observations'],count)
        self.assertEqual(fitted_estimates(self.database,{})['estimates'],[])

    def test_date_validation_inclusive_end_and_unknown_dates(self):
        self.seed([survey_row(0,'2026-01-01'),survey_row(1,'2026-01-02'),survey_row(2,'')])
        self.assertEqual(summary(self.database,{'date_to':['2026-01-01']})['count'],1)
        self.assertEqual(summary(self.database,{'date':['2026-01-01','2026-01-02']})['count'],2)
        self.assertEqual([r['date'] for r in available_dates(self.database,{})['dates']],['2026-01-01','2026-01-02'])
        for params in ({'date_from':['2026-02-30']},{'date':['20260101']},
                       {'date_from':['2026-02-01'],'date_to':['2026-01-01']}):
            with self.assertRaises(ValueError):summary(self.database,params)

    def test_legacy_cached_fits_are_preserved_without_being_reused(self):
        self.seed([survey_row(i,'2026-01-01') for i in range(6)])
        key=radios(self.database,{})['rows'][0]['radio_key']
        with connect(self.database) as db:
            db.execute('DROP TABLE estimates')
            db.execute('CREATE TABLE estimates(radio_key TEXT PRIMARY KEY,result TEXT NOT NULL)')
            db.execute('INSERT INTO estimates VALUES(?,?)',(key,json.dumps({'status':'fitted','latitude':0})))
            db.execute('PRAGMA user_version=2')
        initialize(self.database)
        self.assertEqual(database_info(self.database)['cached_estimates'],1)
        self.assertEqual(fitted_estimates(self.database,{})['estimates'],[])
        with connect(self.database) as db:
            self.assertEqual(db.execute('SELECT scope_key FROM estimates').fetchone()[0],'legacy-full-history-sampled-v1')

    def test_recursive_discovery_deduplicates_paths_and_accepts_uppercase_csv(self):
        nested=self.root/'nested';nested.mkdir()
        source=nested/'SURVEY.CSV';source.write_text('fixture',encoding='utf-8')
        self.assertEqual(discover_csv_files([self.root,source]),[source])

    def test_cancellation_is_checked_inside_model_evaluation(self):
        rows=[{'key':str(i),'mac':MAC,'latitude':48+i*.0001,'longitude':-123+i%2*.001,'rssi_dbm':-70} for i in range(6)]
        model=LocationModel(rows,cancelled=lambda:True)
        with self.assertRaises(InterruptedError):model.fit(np.arange(6),3)


if __name__=='__main__':unittest.main()
