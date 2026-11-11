"""Matched source-safe storage evaluation; never open or migrate source surveys."""
import argparse
import json
from pathlib import Path
import tempfile
import run

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path)
    parser.add_argument('--trials',type=int,default=3)
    parser.add_argument('--requests',type=int,default=200)
    parser.add_argument('--engines',default='before,after,duckdb')
    args=parser.parse_args()
    root=args.root or Path(tempfile.mkdtemp(prefix='phase4-',dir=run.BUILD))
    marker=root/'owner.json'
    if marker.exists():
        if json.loads(marker.read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned benchmark directory required')
    else:run.write_json(marker,{'purpose':'signal-atlas-benchmark','schema':1})
    report={'schema':1,'root':str(root),'trials':args.trials,'requests':args.requests,'threads':8,'duckdb_memory':'1GB','cache_policy':'Process-cold open; filesystem cache uncontrolled','workloads':{}}
    for name in ['cardinality-10m','repeated-10m']:
        source=run.BUILD/'fixtures'/f'{name}.csv';digest=run.sha(source)
        results=[]
        for trial in range(1,args.trials+1):
            for engine in args.engines.split(','):
                if engine not in ['before','after','optimized','final','duckdb']:raise ValueError('Unknown engine')
                prefix=f'{engine}-{name}' if trial==1 else f'{engine}-{name}-{trial}'
                directory=root/prefix
                print(prefix,flush=True)
                if (directory/'result.json').exists():
                    result=json.loads((directory/'result.json').read_text())
                else:
                    if engine=='duckdb':
                        command=[str(run.BUILD/'duckbench-phase4.exe'),'-csv',str(source),'-db',str(root/(prefix+'-data')/'survey.duckdb'),'-memory','1GB','-requests',str(args.requests if trial==1 else 1)]
                    else:
                        binary='baseline-phase4-before.exe' if engine=='before' else 'baseline-phase4-final.exe' if engine=='final' else 'baseline-phase4.exe'
                        command=[str(run.BUILD/binary),'-csv',str(source),'-store',str(root/(prefix+'-data')),'-threads','8','-fits','0','-requests','5','-map-only','-expected-rows','9999900']
                    result=run.execute(command,directory)
                if result['exit_code']!=0 or result.get('stopped_reason'):raise RuntimeError(prefix+' failed')
                results.append({'engine':engine,'trial':trial,**result})
                # Reopen first representative workspaces in independent processes.
                if trial==1 and not (root/(prefix+'-reopen')/'result.json').exists():
                    if engine=='duckdb':command=[str(run.BUILD/'duckbench-phase4.exe'),'-db',str(root/(prefix+'-data')/'survey.duckdb'),'-memory','1GB','-requests','1']
                    else:command=[str(run.BUILD/('baseline-phase4-before.exe' if engine=='before' else 'baseline-phase4-final.exe' if engine=='final' else 'baseline-phase4.exe')),'-store',str(root/(prefix+'-data')),'-reopen-existing','-threads','8','-fits','0','-requests','5','-map-only','-expected-rows','9999900']
                    run.execute(command,root/(prefix+'-reopen'))
            run.write_json(root/'summary-partial.json',report)
        if run.sha(source)!=digest:raise RuntimeError('Original source changed')
        report['workloads'][name]={'sha256':digest,'source_rows':10_000_000,'accepted_rows':9_999_900,'zero_pairs':100,'results':results}
        run.write_json(root/'summary.json',report)
    print(root,flush=True)

if __name__=='__main__':main()
