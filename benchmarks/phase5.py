"""Sequential, complete preparation sweeps after solver/hardware correctness gates."""
import argparse
import hashlib
import json
from pathlib import Path
import sqlite3
import run
from phase2 import clone_workspace


def normalized_digest(path):
    h=hashlib.sha256()
    def normalize(v):
        solver=v.get('solver')
        if solver=='opencl64-reference-v1':v.pop('solver')
        elif solver=='opencl64-fast-v1':v['solver']='fast'
    with sqlite3.connect(path.resolve().as_uri()+'?mode=ro',uri=True)as db:
        for key,scope,result in db.execute('SELECT radio_key,scope_key,result FROM estimates ORDER BY radio_key,scope_key'):
            scope=json.loads(scope);result=json.loads(result)
            normalize(scope);normalize(result['observation_scope']);result.pop('acceleration',None)
            for value in [key,scope,result]:
                h.update(json.dumps(value,sort_keys=True,separators=(',',':'),ensure_ascii=False).encode());h.update(b'\0')
    return h.hexdigest()


def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--root',type=Path,required=True);args=parser.parse_args()
    root=args.root.resolve(strict=True)
    if json.loads((root/'owner.json').read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned run required')
    previous=json.loads((run.ROOT/'benchmarks/baselines/preparation-performance.json').read_text())
    def compact(v):return {k:value for k,value in v.items()if k!='probe'}|{'probe':{k:value for k,value in v['probe'].items()if k not in ['samples','results']}}
    report={'schema':1,'base_commit':'8af4acc','root':str(root),'notes':'Complete unfitted sweeps; native CPU paths retain bitwise results. GPU scopes/provenance normalized only for numeric parity checking; real saved scopes remain separate. Eight Go scheduler slots. Filesystem cache uncontrolled.','baseline_large':compact(previous['native-large-fast']),'baseline_real_reference':compact(previous['native-real-reference'])}
    source=run.BUILD/'phase4-uc_62_q7'
    def execute(name,args):
        print(name,flush=True)
        file=root/name/'result.json'
        value=json.loads(file.read_text())if file.exists()else run.execute(list(map(str,args)),root/name,timeout=3600)
        if value['exit_code']or value.get('stopped_reason'):raise RuntimeError(name+' failed')
        report[name]=value;run.write_json(root/'summary-partial.json',report)
        return value
    for name,fixture in [('gpu-real','real-data'),('gpu-fast-real','real-data'),('cpu-fast-real','real-data'),('gpu-large','final-cardinality-10m-data')]:
        if not(root/(name+'-data')/'store').exists():clone_workspace(source/fixture/'store',root/(name+'-data')/'store')
    tool=run.BUILD/'fullprep-phase5.exe'
    execute('cpu-real-reference',[tool,'-data',root/'after-real-data','-workers','2','-solver','reference'])
    execute('cpu-large-fast',[tool,'-data',root/'after-large-data','-workers','8','-solver','fast'])
    execute('cpu-real-fast',[tool,'-data',root/'cpu-fast-real-data','-workers','2','-solver','fast'])
    execute('gpu-real-reference',[tool,'-data',root/'gpu-real-data','-workers','2','-solver','reference','-gpu'])
    execute('gpu-real-fast',[tool,'-data',root/'gpu-fast-real-data','-workers','2','-solver','fast','-gpu'])
    execute('gpu-large-fast',[tool,'-data',root/'gpu-large-data','-workers','8','-solver','fast','-gpu'])
    paths={'baseline_large':run.BUILD/'preparation-perf-ib58v4ph/native-large-data/store/atlas.sqlite3','cpu_large':root/'after-large-data/store/atlas.sqlite3','gpu_large':root/'gpu-large-data/store/atlas.sqlite3','baseline_reference':run.BUILD/'preparation-perf-ib58v4ph/native-real-data/store/atlas.sqlite3','cpu_reference':root/'after-real-data/store/atlas.sqlite3','gpu_reference':root/'gpu-real-data/store/atlas.sqlite3','cpu_fast_real':root/'cpu-fast-real-data/store/atlas.sqlite3','gpu_fast_real':root/'gpu-fast-real-data/store/atlas.sqlite3'}
    report['normalized_numeric_hashes']={name:normalized_digest(path)for name,path in paths.items()}
    h=report['normalized_numeric_hashes']
    assert h['baseline_large']==h['cpu_large']==h['gpu_large'],'Large outputs differ'
    assert h['baseline_reference']==h['cpu_reference']==h['gpu_reference'],'Reference outputs differ'
    assert h['cpu_fast_real']==h['gpu_fast_real'],'Fast real outputs differ'
    for name in ['gpu-real-reference','gpu-real-fast','gpu-large-fast']:
        status=report[name]['probe']['acceleration']
        assert status['available']and status['calls']>0 and status['fallbacks']==0,(name,status)
    report['integrity']={}
    for name,path in paths.items():
        with sqlite3.connect(path.resolve().as_uri()+'?mode=ro',uri=True)as db:
            check=db.execute('PRAGMA quick_check').fetchall()
            pending=db.execute('SELECT count(*) FROM preparation_pending').fetchone()[0]
            assert check==[('ok',)]and pending==0,(name,check,pending)
            report['integrity'][name]={'quick_check':'ok','pending':pending}
    execute('gpu-reopen',[tool,'-data',root/'gpu-large-data','-workers','8','-solver','fast','-gpu','-verify-existing'])
    report['speedups']={
      'cpu_large_vs_previous':report['baseline_large']['probe']['seconds']/report['cpu-large-fast']['probe']['seconds'],
      'gpu_large_vs_cpu':report['cpu-large-fast']['probe']['seconds']/report['gpu-large-fast']['probe']['seconds'],
      'gpu_reference_vs_cpu':report['cpu-real-reference']['probe']['seconds']/report['gpu-real-reference']['probe']['seconds'],
      'gpu_fast_real_vs_cpu':report['cpu-real-fast']['probe']['seconds']/report['gpu-real-fast']['probe']['seconds']}
    run.write_json(root/'summary.json',report)
    run.write_json(run.ROOT/'benchmarks/baselines/phase5-summary.json',report)
    print(json.dumps(report['speedups']),flush=True)


if __name__=='__main__':main()
