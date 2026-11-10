"""Execute supplemental phase-0 cases sequentially (never concurrently)."""
import argparse
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import run as bm

p=argparse.ArgumentParser()
p.add_argument('--cases',default='real,control,archive,profile,native,dense,export,default,donor')
p.add_argument('--render-data',help='owned completed high-cardinality run data directory')
p.add_argument('--export-data',help='owned completed run data directory for snapshot measurement')
p.add_argument('--probe',default=str(bm.BUILD/'baseline.exe'))
a=p.parse_args()
cases=set(a.cases.split(','))
if cases & {'profile','native','dense','dense-profile'} and not a.render_data:p.error('--render-data is required for map/profile cases')
if 'export' in cases and not a.export_data:p.error('--export-data is required for export')
bm.BUILD.mkdir(parents=True,exist_ok=True)
root=Path(tempfile.mkdtemp(prefix='extra-',dir=bm.BUILD))
bm.write_json(root/'owner.json',{'purpose':'signal-atlas-benchmark','schema':1})
index={}
probe=str(Path(a.probe).resolve())

def execute(name,command,**kwargs):
    print(name,flush=True)
    result=bm.execute(command,root/name,**kwargs)
    index[name]=str(root/name/'result.json')
    bm.write_json(root/'index.json',index)
    return result

for case in a.cases.split(','):
    if case=='real':
        oracle=json.loads((bm.BUILD/'real-inputs.json').read_text())
        files=';'.join(str((bm.ROOT.parent/'inputs'/f['name']).resolve()) for f in oracle['files'])
        execute('real-inputs',[probe,'-csv',files,'-store',str(root/'real-inputs'/'data'),'-threads','8','-requests','200','-fits','64','-expected-rows',str(oracle['total_valid_rows'])])
    elif case=='control':
        execute('incremental',[str(bm.BUILD/'controlbench.exe'),'-csv',str(bm.BUILD/'fixtures'/'repeated-1m.csv'),'-data',str(root/'incremental'/'data')])
    elif case=='archive':
        execute('archive-1m',[probe,'-csv',str(bm.BUILD/'fixtures'/'repeated-1m.csv'),'-store',str(root/'archive-1m'/'data'),'-threads','8','-requests','1','-fits','0','-archive-only'])
    elif case in {'profile','dense-profile'}:
        command=[probe,'-store',str((bm.ROOT/a.render_data).resolve()),'-reopen-existing','-threads','8','-requests','20','-fits','0','-cpuprofile',str(root/case/'cpu.pprof'),'-memprofile',str(root/case/'heap.pprof')]
        if case=='dense-profile':command.append('-map-only')
        execute(case,command)
        with (root/case/'cpu-top.txt').open('w') as out:
            subprocess.run(['go','tool','pprof','-top','-nodecount=25',str(root/case/'cpu.pprof')],cwd=bm.ROOT,stdout=out,stderr=subprocess.STDOUT,check=True)
    elif case=='native':
        for page in ['gpu','explorer','donor']:
            before=set(bm.BUILD.glob('native-'+page+'-*'))
            cmd=[sys.executable,str(bm.ROOT/'benchmarks'/'run.py'),'native','--data',a.render_data,'--page',page,'--probe',str(bm.BUILD/'nativebench.exe'),'--donor',str(bm.ROOT/'build'/'benchmarks'/'sources-epq2r4mk'/'donor')]
            if page=='gpu':cmd+=['--seed-fits','50000']
            subprocess.run(cmd,cwd=bm.ROOT,check=True)
            created=set(bm.BUILD.glob('native-'+page+'-*'))-before
            index['native-'+page]=str(next(iter(created))/'result.json')
            bm.write_json(root/'index.json',index)
    elif case=='dense':
        execute('dense-ap',[probe,'-store',str((bm.ROOT/a.render_data).resolve()),'-reopen-existing','-map-only','-threads','8','-requests','200','-fits','0'])
    elif case=='export':
        execute('export-10m',[probe,'-store',str((bm.ROOT/a.export_data).resolve()),'-reopen-existing','-threads','8','-requests','1','-fits','0','-export'])
    elif case=='default':
        execute('default-10m',[probe,'-csv',str(bm.BUILD/'fixtures'/'repeated-10m.csv'),'-store',str(root/'default-10m'/'data'),'-threads','16','-requests','1','-fits','0','-expected-rows','10000000'])
    elif case=='donor':
        for stem,trials,fits,requests in [('repeated-10m',3,8,200),('cardinality-10m',3,8,5),('capacity-50m',1,0,5)]:
            before=set(bm.BUILD.glob('run-*'))
            bm.run(SimpleNamespace(csv=str(bm.BUILD/'fixtures'/(stem+'.csv')),manifest=str(bm.BUILD/'fixtures'/(stem+'.json')),probe=str(bm.BUILD/'donor-baseline.exe'),trials=trials,threads=8,requests=requests,fits=fits,max_rss_gib=12,bbox='-123.401,48.399,-123.390,48.410',export=False,archive=False))
            created=set(bm.BUILD.glob('run-*'))-before
            index['donor-'+stem]=str(next(iter(created))/'baseline.json')
            bm.write_json(root/'index.json',index)
    elif case=='cold-donor':
        selected={}
        for path in bm.BUILD.glob('run-*/baseline.json'):
            report=json.loads(path.read_text());trial=report['trials'][0]
            if trial.get('probe',{}).get('engine')!='donor-duckdb':continue
            stem=trial['probe']['imports'][0]['file'].removesuffix('.csv')
            args=trial['command'];selected[stem]=args[args.index('-store')+1]
        for stem,data in selected.items():
            execute('cold-donor-'+stem,[str(bm.BUILD/'donor-baseline.exe'),'-store',data,'-reopen-existing','-threads','8','-requests','1','-fits','0'])
    else:raise ValueError('unknown case '+case)
print(root/'index.json')
