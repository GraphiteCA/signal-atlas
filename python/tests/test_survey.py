"""Large-survey ingestion, filtering, grouping and HTTP behavior."""
import csv
import json
import math
from pathlib import Path
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from survey_data import (backup_database, connect, database_info, import_csv, infer_band, initialize,
                         load_basemap, networks, observations, points, radios, store_basemap, summary, transmitters)
from serve_survey import Jobs, SurveyServer


FIELDS = ['id','SSID','MAC','RSSI','CurrentLatitude','CurrentLongitude','Frequency','Channel','FirstSeen','AccuracyMeters']


def write_csv(path, rows):
    with path.open('w', encoding='utf-8', newline='') as handle:
        writer = csv.DictWriter(handle, fieldnames=FIELDS)
        writer.writeheader()
        writer.writerows(rows)


def row(i, ssid='Shared network', mac='02:00:00:00:00:01', frequency='2412', lat=None, lon=None):
    return dict(id=str(i),SSID=ssid,MAC=mac,RSSI=-60-i%30,CurrentLatitude=lat if lat is not None else 48.449+i*.00001,
                CurrentLongitude=lon if lon is not None else 2.668+i*.00001,Frequency=frequency,Channel='1',
                FirstSeen='2026-11-08 12:00:00',AccuracyMeters=3.8)


class SurveyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.database = self.root/'survey.sqlite3'
        self.source = self.root/'data.csv'

    def tearDown(self):
        self.temp.cleanup()

    def seed(self, rows):
        write_csv(self.source, rows)
        return import_csv(self.source, self.database)

    def test_band_units_and_ambiguity(self):
        for frequency in ('2412', '2.412 GHz', '2412000 kHz', '2412000000 Hz'):
            self.assertEqual(infer_band(frequency)[0], '2.4')
        self.assertEqual(infer_band('5180')[0], '5')
        self.assertEqual(infer_band('6.115 GHz')[0], '6')
        self.assertEqual(infer_band('', '5 GHz')[0], '5')
        self.assertEqual(infer_band('2412', '5 GHz')[0], 'unknown')
        self.assertEqual(infer_band('')[0], 'unknown')

    def test_wigle_preamble_and_non_wifi_rows(self):
        self.source.write_text('WigleWifi-1.4,appRelease=test\nMAC,SSID,RSSI,CurrentLatitude,CurrentLongitude,Frequency,Type\n'
                               '02:00:00:00:00:01,WiFi,-70,48.449,2.668,5180,WIFI\n'
                               '02:00:00:00:00:02,Bluetooth,-50,48.449,2.668,,BLE\n',encoding='utf-8')
        result=import_csv(self.source,self.database)
        self.assertEqual(result['valid_rows'],1)
        self.assertEqual(result['non_wifi_rows'],1)
        self.assertEqual(radios(self.database,{})['rows'][0]['band'],'5')

    def test_same_ssid_has_distinct_bssids_and_bands(self):
        self.seed([row(1), row(2, mac='02:00:00:00:00:02', frequency='5180'),
                   row(3, frequency='5180'), row(4, ssid='Other')])
        self.assertEqual(summary(self.database)['count'], 4)
        result = radios(self.database, {'ssid':['Shared network']})['rows']
        self.assertEqual(len(result), 3)
        self.assertEqual({r['band'] for r in result}, {'2.4','5'})
        self.assertEqual(summary(self.database, {'band':['5']})['count'], 2)

    def test_multiple_ssid_selection_and_exclusions(self):
        self.seed([row(1,ssid='A'),row(2,ssid='B'),row(3,ssid='C')])
        self.assertEqual(summary(self.database, {'ssid':['A','C']})['count'],2)
        self.assertEqual(summary(self.database, {'exclude_ssid':['B']})['count'],2)
        self.assertEqual(summary(self.database, {'none':['1']})['count'],0)
        key=radios(self.database,{'ssid':['A']})['rows'][0]['radio_key']
        self.assertEqual(summary(self.database,{'exclude_radio':[key]})['count'],2)

    def test_mac_choices_are_scoped_to_their_parent_ssid(self):
        first,second='02:00:00:00:00:01','02:00:00:00:00:02'
        self.seed([row(1,ssid='A',mac=first),row(2,ssid='A',mac=second),
                   row(3,ssid='B',mac=first),row(4,ssid='B',mac=second)])
        params={'mac_selection':[json.dumps({'A':{'mode':'include','macs':[first]}})]}
        result=observations(self.database,params)['rows']
        self.assertEqual({(r['ssid'],r['mac']) for r in result},{('A',first),('B',first),('B',second)})
        params={'mac_selection':[json.dumps({'A':{'mode':'exclude','macs':[first]}})]}
        self.assertEqual(summary(self.database,params)['count'],3)
        params={'mac_selection':[json.dumps({'A':{'mode':'include','macs':[]}})]}
        self.assertEqual(summary(self.database,params)['count'],2)
        self.assertEqual(summary(self.database,{'ssid':['A'],'mac':[first.lower()]})['count'],1)

    def test_mac_catalog_groups_bands_and_supports_scoped_search(self):
        mac='02:00:00:00:AA:01'
        self.seed([row(1,ssid='A',mac=mac),row(2,ssid='A',mac=mac,frequency='5180'),
                   row(3,ssid='A',mac='02:00:00:00:AA:02'),row(4,ssid='B',mac=mac)])
        result=transmitters(self.database,{'ssid':['A'],'q':['aa:01']})['rows']
        self.assertEqual(len(result),1)
        self.assertEqual(result[0]['count'],2)
        self.assertEqual(result[0]['radio_count'],2)
        self.assertEqual(set(result[0]['bands'].split(',')),{'2.4','5'})
        self.assertEqual(networks(self.database,{'q':['A']})['rows'][0]['macs'],2)

    def test_invalid_mac_selection_is_rejected(self):
        self.seed([row(1)])
        for choices in ([],{'A':{'mode':'invalid','macs':[]}}, {'A':{'mode':'include','macs':'not-a-list'}}):
            with self.assertRaises(ValueError):summary(self.database,{'mac_selection':[json.dumps(choices)]})

    def test_existing_database_migrates_without_losing_observations(self):
        self.seed([row(1),row(2,ssid='Other')])
        with connect(self.database) as db:
            for name in ('network_catalog','transmitter_catalog','radio_catalog'):
                db.execute(f'DROP VIEW {name}')
            db.execute('PRAGMA user_version=0')
        initialize(self.database)
        info=database_info(self.database)
        self.assertEqual(info['schema_version'],5)
        self.assertEqual(info['observations'],2)
        self.assertEqual(info['transmitters'],2)

    def test_unrelated_sqlite_store_is_not_modified(self):
        with connect(self.database) as db:
            db.execute('CREATE TABLE unrelated(value TEXT)')
        with self.assertRaisesRegex(ValueError,'not a Signal Atlas'):
            initialize(self.database)
        with connect(self.database) as db:
            names={r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            self.assertEqual(names,{'unrelated'})

    def test_backup_includes_committed_wal_data_and_refuses_overwrites(self):
        self.seed([row(1)])
        snapshot=self.root/'snapshot.sqlite3'
        # Keep a connection open so its committed changes can remain in WAL.
        with connect(self.database) as source:
            source.execute('INSERT INTO estimates(radio_key,scope_key,result) VALUES(?,?,?)',('cached','test-scope','{"status":"fitted"}'))
            source.commit()
            backup_database(self.database,snapshot)
        self.assertEqual(database_info(snapshot)['observations'],1)
        self.assertEqual(database_info(snapshot)['cached_estimates'],1)
        with self.assertRaises(FileExistsError):backup_database(self.database,snapshot)
        with self.assertRaises(ValueError):backup_database(self.database,self.database)

    def test_database_snapshot_keeps_map_context_without_the_sidecar(self):
        self.seed([row(1)])
        geojson=self.root/'context.geojson'
        payload={'type':'FeatureCollection','features':[], 'metadata':{'attribution':'Test fixture'}}
        geojson.write_text(json.dumps(payload),encoding='utf-8')
        store_basemap(self.database,geojson)
        snapshot=backup_database(self.database,self.root/'portable.sqlite3')
        geojson.unlink()
        self.source.unlink()
        self.assertEqual(load_basemap(snapshot),payload)
        self.assertTrue(database_info(snapshot)['has_basemap'])

    def test_idempotent_import_and_append_invalidates_estimates(self):
        self.seed([row(1)])
        self.assertTrue(import_csv(self.source,self.database)['duplicate'])
        with connect(self.database) as db:
            db.execute('INSERT INTO estimates(radio_key,scope_key,result) VALUES(?,?,?)',('cached','test-scope','{}'))
        write_csv(self.source,[row(2)])
        import_csv(self.source,self.database)
        self.assertEqual(summary(self.database)['count'],2)
        with connect(self.database) as db:
            self.assertEqual(db.execute('SELECT count(*) FROM estimates').fetchone()[0],0)

    def test_failed_import_rolls_back_all_rows_and_metadata(self):
        rows=[row(i) for i in range(5001)]
        rows.append(dict(row(5002),CurrentLatitude='nan'))
        write_csv(self.source,rows)
        with self.assertRaises(ValueError):import_csv(self.source,self.database)
        self.assertEqual(summary(self.database)['count'],0)
        with connect(self.database) as db:
            self.assertEqual(db.execute('SELECT count(*) FROM imports').fetchone()[0],0)
            self.assertEqual(db.execute('SELECT count(*) FROM spatial').fetchone()[0],0)
        result=import_csv(self.source,self.database,skip_invalid=True)
        self.assertEqual(result['valid_rows'],5001)
        self.assertEqual(result['invalid_rows'],1)

    def test_spatial_query_uses_exact_coordinates_after_rtree(self):
        self.seed([row(1,lat=48.449,lon=2.668),row(2,lat=48.4490001,lon=2.668)])
        selected=points(self.database,{'bbox':['2.667,48.4489,2.669,48.44900005']})
        self.assertEqual(selected['count'],1)

    def test_aggregation_retains_counts_with_bounded_payload(self):
        self.seed([row(i,ssid='A' if i%2 else 'B') for i in range(6000)])
        result=points(self.database,{'limit':['100']})
        self.assertTrue(result['aggregated'])
        self.assertLessEqual(len(result['points']),100)
        self.assertEqual(sum(p['count'] for p in result['points']),6000)
        raw=observations(self.database,{'limit':['50'],'offset':['50']})
        self.assertEqual(len(raw['rows']),50)
        self.assertTrue(raw['more'])
        self.assertEqual(raw['rows'][0]['record_id'],'50')

    def test_quoted_ssids_and_literal_search_are_preserved(self):
        name='Bob\'s, "Wi-Fi" 100%_'
        self.seed([row(1,ssid=name),row(2,ssid='Other')])
        self.assertEqual(networks(self.database,{'q':['100%_']})['rows'][0]['ssid'],name)
        self.assertEqual(summary(self.database,{'ssid':[name]})['count'],1)

    def test_independent_radios_recover_separate_transmitters(self):
        rows=[]
        for radio, base in enumerate((48.449,48.453)):
            for i in range(12):
                angle=i*math.pi/6
                lat=base+math.sin(angle)*60/111195.08
                lon=2.668+math.cos(angle)*60/(111195.08*math.cos(math.radians(base)))
                rows.append(dict(row(len(rows),mac=f'02:00:00:00:00:0{radio+1}',lat=lat,lon=lon),RSSI=-38-30*math.log10(60)))
        self.seed(rows)
        jobs=Jobs(self.database)
        jobs.estimate({})
        deadline=time.monotonic()+15
        while jobs.snapshot()['status']=='running' and time.monotonic()<deadline:time.sleep(.01)
        self.assertEqual(jobs.snapshot()['status'],'complete')
        with connect(self.database) as db:fits=[json.loads(r[0]) for r in db.execute('SELECT result FROM estimates ORDER BY radio_key')]
        self.assertEqual(len(fits),2)
        self.assertAlmostEqual(fits[0]['latitude'],48.449,places=4)
        self.assertAlmostEqual(fits[1]['latitude'],48.453,places=4)
        self.assertFalse(fits[0]['co_location_assumed'])

    def test_local_http_api_and_origin_guard(self):
        self.seed([row(1),row(2,ssid='Other')])
        server=SurveyServer(('127.0.0.1',0),self.database)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        base=f'http://127.0.0.1:{server.server_port}'
        try:
            with urllib.request.urlopen(base+'/api/summary') as response:
                self.assertEqual(json.load(response)['count'],2)
            with urllib.request.urlopen(base+'/') as response:
                self.assertIn(b'Find the signal',response.read())
            with urllib.request.urlopen(base+'/api/area-estimates?zoom=15') as response:
                self.assertTrue(json.load(response)['zoom_required'])
            with urllib.request.urlopen(base+'/area_estimates.js') as response:
                self.assertIn(b'scheduleAreaEstimates', response.read())
            request=urllib.request.Request(base+'/api/area-estimate?zoom=15',method='POST')
            with urllib.request.urlopen(request) as response:
                self.assertEqual(json.load(response)['status'], 'idle')
            # Individual overview fits stay opt-in even for direct HTTP calls.
            request=urllib.request.Request(base+'/api/area-estimate?zoom=17&bbox=2.66,48.44,2.68,48.46',method='POST')
            with urllib.request.urlopen(request) as response:
                self.assertEqual(json.load(response)['status'], 'idle')
            request=urllib.request.Request(base+'/api/estimate',method='POST',headers={'Origin':'https://unrelated.example'})
            with self.assertRaises(urllib.error.HTTPError) as caught:urllib.request.urlopen(request)
            self.assertEqual(caught.exception.code,403)
        finally:
            server.shutdown();server.server_close();thread.join()


if __name__=='__main__':unittest.main()
