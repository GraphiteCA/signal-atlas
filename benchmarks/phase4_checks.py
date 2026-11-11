"""Additional Phase 4 probes; run after imports, without concurrent compilers/tests."""
import argparse
import json
from pathlib import Path
import run

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',required=True,type=Path)
    parser.add_argument('--capacity',action='store_true')
    parser.add_argument('--map-source',type=Path,default=run.BUILD/'run-hs077f3e/trial-1/store/store')
    parser.add_argument('--sql-source',type=Path,default=run.BUILD/'phase3-g_ifubvu/small-data/store')
    args=parser.parse_args();root=args.root.resolve(strict=True)
    marker=json.loads((root/'owner.json').read_text())
    if marker!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned benchmark required')
    from phase2 import clone_workspace
    import sqlite3
    for name,source in [('map-data',args.map_source),('sql-native-data',args.sql_source),('sql-browser-data',args.sql_source)]:
        if not (root/name/'store').exists():
            clone_workspace(source,root/name/'store')
            if name.startswith('sql-'):
                with sqlite3.connect(root/name/'store/atlas.sqlite3') as c:
                    c.execute("INSERT OR REPLACE INTO map_assets VALUES('preparation_settings',?)",(json.dumps({'version':1,'enabled':False,'paused':True,'fit_mode':'sample','grouping':'shared','workers':2,'memory_mb':256}),))
    report={'schema':1,'root':str(root)}
    def execute(name,command,allow_failure=False,**options):
        print(name,flush=True)
        dest=root/name
        if (dest/'result.json').exists():result=json.loads((dest/'result.json').read_text())
        else:
            try:result=run.execute(list(map(str,command)),dest,**options)
            except RuntimeError:
                if not allow_failure:raise
                result=json.loads((dest/'result.json').read_text())
        report[name]=result;run.write_json(root/'checks-summary.json',report)
        return result
    for profile in ['cardinality-10m','repeated-10m']:
        execute('sql-'+profile,[run.BUILD/'storagebench-phase4.exe','-data',root/((('final-'if (root/('final-'+profile+'-data')).exists()else 'optimized-')+profile+'-data'))/'store','-requests','200'])
    # A tuning pilot prevents comparing only an untuned, unindexed candidate.
    # Index construction is measured separately; add it to import commit time
    # when comparing a fresh, query-ready indexed database.
    execute('duckdb-indexed-cardinality',[run.BUILD/'duckbench-phase4-index.exe','-csv',run.BUILD/'fixtures/cardinality-10m.csv','-db',root/'duckdb-indexed-data/survey.duckdb','-memory','1GB','-requests','200','-index','-expected-rows','9999900'],allow_failure=True)
    execute('map',[run.BUILD/'mapbench-phase4.exe','-data',root/'map-data','-requests','200'])
    if args.capacity:
        source=run.BUILD/'fixtures/capacity-50m.csv';before=run.sha(source)
        execute('capacity-column',[run.BUILD/'baseline-phase4.exe','-csv',source,'-store',root/'capacity-column-data','-threads','8','-fits','0','-requests','1','-map-only','-expected-rows','49999500'])
        execute('capacity-duckdb',[run.BUILD/'duckbench-phase4-index.exe','-csv',source,'-db',root/'capacity-duckdb-data/survey.duckdb','-memory','1GB','-requests','1','-expected-rows','49999500'])
        if run.sha(source)!=before:raise RuntimeError('Capacity source changed')
        report['capacity_sha256']=before
    sources=sorted((run.ROOT.parent/'inputs').glob('*.csv'))
    if sources:
        hashes={str(p):run.sha(p) for p in sources}
        execute('real-imports',[run.BUILD/'baseline-phase4.exe','-csv',';'.join(map(str,sources)),'-store',root/'real-data','-threads','8','-fits','0','-requests','5','-map-only','-expected-rows','175447'])
        if hashes!={str(p):run.sha(p) for p in sources}:raise RuntimeError('Original scan source changed')
        report['real_input_hashes']=hashes
    execute('browser-sql',['python',run.ROOT/'benchmarks/visual.py','--data',root/'sql-browser-data','--app',run.ROOT/'build/SignalAtlas-phase4.exe','--driver','sql-smoke.cjs'],json_output=False,timeout=180)
    for name,page,driver,data in [('native-sql','sql','sql-native.js','sql-native-data'),('native-map','gpu','phase1-native.js','map-data')]:
        output=root/(name+'.json')
        execute(name,[run.BUILD/'nativebench-phase4.exe','-repo',run.ROOT,'-data',root/data,'-page',page,'-driver',driver,'-output',output],json_output=False,family=True,max_rss_bytes=6*1024**3,timeout=180)
        value=json.loads(output.read_text(encoding='utf-8'))
        if value.get('success') is False or value.get('errors'):raise RuntimeError(name+' failed: '+json.dumps(value))
        report[name]['ui_result']=value
    run.write_json(root/'checks-summary.json',report)
    print(root,flush=True)

if __name__=='__main__':main()
