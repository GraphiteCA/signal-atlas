"""Reproducible large-survey benchmark; generated observations are synthetic."""
import argparse
import csv
import json
import math
from pathlib import Path
import sys
import tempfile
import time
import tracemalloc
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from survey_data import import_csv, points, summary
from serve_survey import Jobs


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rows',type=int,default=300000)
    parser.add_argument('--output',type=Path)
    args=parser.parse_args()
    with tempfile.TemporaryDirectory() as temp:
        source,database=Path(temp)/'synthetic.csv',Path(temp)/'synthetic.sqlite3'
        with source.open('w',newline='',encoding='utf-8') as f:
            writer=csv.writer(f);writer.writerow(['id','SSID','MAC','RSSI','CurrentLatitude','CurrentLongitude','Frequency','FirstSeen'])
            for i in range(args.rows):
                radio=i%200;angle=i*2.399963229728653;r=20+(i%140)
                lat=48.445+(radio//20)*.0006+math.sin(angle)*r/111195.08
                lon=2.662+(radio%20)*.0006+math.cos(angle)*r/(111195.08*.663)
                writer.writerow([i,f'Network {radio//2:03}',f'02:00:00:00:{radio//256:02X}:{radio%256:02X}',round(-38-30*math.log10(r),2),round(lat,8),round(lon,8),2412 if radio%2==0 else 5180,'2026-11-08 12:00:00'])
        tracemalloc.start();start=time.perf_counter();result=import_csv(source,database);import_seconds=time.perf_counter()-start
        _,peak=tracemalloc.get_traced_memory();tracemalloc.stop()
        start=time.perf_counter();stats=summary(database);summary_seconds=time.perf_counter()-start
        start=time.perf_counter();display=points(database,{'limit':['2000']});query_seconds=time.perf_counter()-start
        assert stats['count']==args.rows
        assert len(display['points'])<=2000
        assert sum(p['count'] for p in display['points'])==args.rows
        with __import__('survey_data').connect(database) as db:group=dict(db.execute('SELECT radio_key,ssid,mac,band,count(*) count FROM observations GROUP BY radio_key LIMIT 1').fetchone())
        start=time.perf_counter();fit=Jobs(database).fit_radio(group,{'fit_mode':['sample']});fit_seconds=time.perf_counter()-start
        assert fit['sampled_observations']<=256 and fit['status']=='fitted'
        report={'synthetic_rows':args.rows,'ssids':stats['networks'],'radios':stats['radios'],'import_seconds':round(import_seconds,3),
                'import_peak_python_traced_mb':round(peak/1024**2,2),'summary_seconds':round(summary_seconds,3),
                'map_query_seconds':round(query_seconds,3),'map_marks':len(display['points']),
                'map_response_kb':round(len(json.dumps(display).encode())/1024,2),
                'one_radio_fit_seconds':round(fit_seconds,3),'fit_sample_count':fit['sampled_observations'],
                'notes':'Synthetic data; timings depend on hardware. Traced Python allocations exclude SQLite/NumPy native allocations.'}
        print(json.dumps(report,indent=2))
        if args.output:args.output.write_text(json.dumps(report,indent=2)+'\n',encoding='utf-8')


if __name__=='__main__':main()
