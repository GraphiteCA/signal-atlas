"""Prepare owned Phase 3 fixtures and measure background priority without changing source surveys."""
import csv
import json
from pathlib import Path
import tempfile
import sqlite3
import run
from phase2 import clone_workspace

def main():
    root=Path(tempfile.mkdtemp(prefix='phase3-',dir=run.BUILD))
    run.write_json(root/'owner.json',{'purpose':'signal-atlas-benchmark','schema':1})
    clone_workspace(run.BUILD/'run-hs077f3e'/'trial-1'/'store'/'store',root/'active-data'/'store')
    source=root/'small.csv'
    with source.open('w',newline='',encoding='utf-8') as f:
        writer=csv.writer(f);writer.writerow(['MAC','SSID','FirstSeen','Frequency','RSSI','CurrentLatitude','CurrentLongitude','AccuracyMeters','Type'])
        for network in range(64):
            for i in range(8):
                writer.writerow([f'02:73:00:00:00:{network:02X}',f'Prepared-{network:03}','2026-11-01 00:00:00',2412,-55-i,48.4+i*.0001,-123.4+(i%3)*.0001,3,'WIFI'])
    report={'schema':1,'root':str(root)}
    report['large_active']=run.execute([str(run.BUILD/'preparationbench-phase3.exe'),'-data',str(root/'active-data'),'-requests','60'],root/'large-active')
    report['small_import']=run.execute([str(run.BUILD/'baseline-phase3.exe'),'-csv',str(source),'-store',str(root/'small-data'),'-expected-rows','512','-threads','8','-fits','0','-requests','1','-map-only'],root/'small-import')
    # Fresh complete workspace copies for independent native and browser tests.
    clone_workspace(root/'small-data'/'store',root/'native-data'/'store')
    clone_workspace(root/'small-data'/'store',root/'browser-data'/'store')
    for name in ['native-data','browser-data']:
        with sqlite3.connect(root/name/'store'/'atlas.sqlite3') as db:
            db.execute("INSERT OR REPLACE INTO map_assets VALUES('preparation_settings',?)",(json.dumps({'version':1,'enabled':True,'paused':True,'fit_mode':'sample','grouping':'shared','workers':2,'memory_mb':256}),))
    source=run.BUILD/'fixtures'/'cardinality-10m.csv'
    digest=run.sha(source)
    report['import_10m']=run.execute([str(run.BUILD/'baseline-phase3.exe'),'-csv',str(source),'-store',str(root/'import-data'),'-expected-rows','9999900','-threads','8','-fits','0','-requests','5','-map-only'],root/'import-10m')
    if run.sha(source)!=digest:raise RuntimeError('Original fixture changed')
    report['input_sha256']=digest
    run.write_json(root/'summary.json',report)
    print(root)

if __name__=='__main__': main()
