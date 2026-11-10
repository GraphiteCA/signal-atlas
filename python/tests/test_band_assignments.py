"""Manual declarations fill absent metadata without inventing measured bands."""
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from survey_data import (assign_band,backup_database,connect,import_csv,observations,
                         radios,summary,transmitters)
from test_survey import row,write_csv


class BandAssignmentTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.root=Path(self.temp.name)
        self.database=self.root/'survey.sqlite3';self.source=self.root/'survey.csv'
        self.mac='02:00:00:00:00:01';self.ssid='Shared network'
        write_csv(self.source,[row(i,frequency='') for i in range(4)]+
                  [row(i+4,frequency='5180') for i in range(4)]+
                  [row(20,ssid='Other',frequency='')])
        import_csv(self.source,self.database)

    def tearDown(self):self.temp.cleanup()

    def test_assignment_updates_tags_filters_keys_and_provenance(self):
        result=assign_band(self.database,self.ssid,self.mac,'2.4')
        self.assertEqual(result['updated'],4)
        self.assertEqual(summary(self.database,{'ssid':[self.ssid],'band':['2.4']})['count'],4)
        rows=observations(self.database,{'ssid':[self.ssid],'band':['2.4']})['rows']
        self.assertTrue(all(r['band_source']=='manual assignment' and r['frequency_mhz'] is None for r in rows))
        self.assertTrue(all('2.4' in r['radio_key'] for r in rows))
        self.assertEqual(summary(self.database,{'ssid':['Other'],'band':['unknown']})['count'],1)
        child=transmitters(self.database,{'ssid':[self.ssid]})['rows'][0]
        self.assertEqual(child['manual_band'],'2.4')
        self.assertEqual(set(child['bands'].split(',')),{'2.4','5'})

    def test_measured_frequency_is_never_replaced_and_clear_restores_missing(self):
        assign_band(self.database,self.ssid,self.mac,'2.4')
        measured=observations(self.database,{'ssid':[self.ssid],'band':['5']})['rows']
        self.assertTrue(all(r['band_source']=='frequency' and r['frequency_mhz']==5180 for r in measured))
        assign_band(self.database,self.ssid,self.mac,'')
        self.assertEqual(summary(self.database,{'ssid':[self.ssid],'band':['unknown']})['count'],4)
        self.assertIsNone(transmitters(self.database,{'ssid':[self.ssid]})['rows'][0]['manual_band'])

    def test_assignment_survives_backup_and_applies_to_future_missing_observations(self):
        assign_band(self.database,self.ssid,self.mac,'2.4')
        snapshot=backup_database(self.database,self.root/'saved.sqlite3')
        self.assertEqual(transmitters(snapshot,{'ssid':[self.ssid]})['rows'][0]['manual_band'],'2.4')
        write_csv(self.source,[row(30,frequency='')]);import_csv(self.source,self.database)
        self.assertEqual(summary(self.database,{'ssid':[self.ssid],'band':['2.4']})['count'],5)

    def test_invalid_assignments_are_rejected_without_changing_data(self):
        for values in ((self.ssid,self.mac,'9'),(None,self.mac,'5'),(self.ssid,'missing','5')):
            with self.assertRaises(ValueError):assign_band(self.database,*values)
        self.assertEqual(summary(self.database)['count'],9)
        with connect(self.database) as db:self.assertEqual(db.execute('SELECT count(*) FROM band_assignments').fetchone()[0],0)


if __name__=='__main__':unittest.main()
