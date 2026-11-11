"""Verify small appends on independent copies of the final 10M-row survey."""
import argparse
import csv
import json
from pathlib import Path
import run
from phase2 import clone_workspace

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,required=True)
    args=parser.parse_args();root=args.root.resolve(strict=True)
    if json.loads((root/'owner.json').read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned benchmark required')
    source=root/'small-100.csv'
    if not source.exists():
        with source.open('w',newline='',encoding='utf-8')as f:
            w=csv.writer(f);w.writerow(['MAC','SSID','FirstSeen','Frequency','RSSI','CurrentLatitude','CurrentLongitude','AccuracyMeters','Type'])
            for i in range(100):w.writerow(['02:44:00:00:00:01','Phase4-incremental','2026-11-10 00:00:00',2412,-60-i%20,48.4+i*.000001,-123.4,3,'WIFI'])
    digest=run.sha(source)
    report={'schema':1,'input_sha256':digest,'trials':[]}
    for trial in range(1,4):
        target=root/f'incremental-final-data-{trial}'/'store'
        clone_workspace(root/'final-cardinality-10m-data/store',target)
        result=run.execute([str(run.BUILD/'storagebench-phase4-final.exe'),'-data',str(target),'-import',str(source),'-requests','1'],root/f'incremental-final-{trial}')
        if result['probe']['rows']!=10_000_000 or result['probe']['incremental_import']['outcome']['valid_rows']!=100:raise RuntimeError('Append count mismatch')
        report['trials'].append(result)
        print(trial,result['probe']['incremental_import']['wall_seconds'],flush=True)
    reopened=run.execute([str(run.BUILD/'storagebench-phase4-final.exe'),'-data',str(target),'-requests','1'],root/'incremental-final-reopen')
    if reopened['probe']['rows']!=10_000_000:raise RuntimeError('Reopen count mismatch')
    report['reopen']=reopened
    if run.sha(source)!=digest:raise RuntimeError('Source changed')
    run.write_json(root/'append-summary.json',report)

if __name__=='__main__':main()
