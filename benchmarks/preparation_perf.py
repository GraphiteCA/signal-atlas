"""Full preparation comparison after isolated before-runs and correctness checks."""
import argparse
import hashlib
import json
from pathlib import Path
import sqlite3
import run
from phase2 import clone_workspace

def digest_estimates(path):
    digest=hashlib.sha256()
    with sqlite3.connect(path.resolve().as_uri()+'?mode=ro',uri=True)as c:
        for row in c.execute('SELECT radio_key,scope_key,result FROM estimates ORDER BY radio_key,scope_key'):
            for value in row:digest.update(value.encode());digest.update(b'\0')
    return digest.hexdigest()

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,required=True)
    args=parser.parse_args();root=args.root.resolve(strict=True)
    if json.loads((root/'owner.json').read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned benchmark required')
    before=json.loads((root/'before-fast-full/result.json').read_text())
    real_before=json.loads((root/'before-real-full/result.json').read_text())
    if before['exit_code'] or real_before['exit_code']:raise RuntimeError('Before sweeps must complete')
    report={'schema':1,'root':str(root),'before_fast':before,'before_real_reference':real_before,'notes':'Complete sweeps; fast/sample/shared on large data, reference/sample/shared on real inputs. Same solver and typed source rows across observation backends. Eight scheduler slots. Filesystem cache uncontrolled.'}
    source=run.BUILD/'phase4-uc_62_q7/final-cardinality-10m-data/store'
    real_source=run.BUILD/'phase4-uc_62_q7/real-data/store'
    def execute(name,command,**options):
        print(name,flush=True)
        existing=root/name/'result.json'
        if existing.exists():value=json.loads(existing.read_text())
        else:value=run.execute(list(map(str,command)),root/name,timeout=3600,**options)
        if value['exit_code'] or value.get('stopped_reason'):raise RuntimeError(name+' failed')
        report[name]=value;run.write_json(root/'summary-partial.json',report);return value
    if not(root/'canonical-source/store').exists():clone_workspace(source,root/'canonical-source/store')
    if not(root/'native-large-data/store').exists():clone_workspace(source,root/'native-large-data/store')
    if not(root/'native-real-data/store').exists():clone_workspace(real_source,root/'native-real-data/store')
    if not(root/'duck-metadata/preparation.sqlite3').exists():
        (root/'duck-metadata').mkdir()
        with sqlite3.connect(source.resolve().joinpath('atlas.sqlite3').as_uri()+'?mode=ro',uri=True)as original,sqlite3.connect(root/'duck-metadata/preparation.sqlite3')as dest:original.backup(dest)
    execute('canonical-setup',[run.BUILD/'duckprep.exe','-db',root/'canonical.duckdb','-build-from',root/'canonical-source/store'])
    native_real=execute('native-real-reference',[run.BUILD/'fullprep-after.exe','-data',root/'native-real-data','-workers','2','-solver','reference'])
    native=execute('native-large-fast',[run.BUILD/'fullprep-after.exe','-data',root/'native-large-data','-workers','8','-solver','fast'])
    duck=execute('duck-large-fast',[run.BUILD/'duckprep.exe','-db',root/'canonical.duckdb','-metadata',root/'duck-metadata/preparation.sqlite3','-workers','8','-solver','fast','-batch','256'])
    old_digest=digest_estimates(root/'before-fast-data/store/atlas.sqlite3')
    real_digest=digest_estimates(root/'before-real-data/store/atlas.sqlite3')
    report['exact_result_hashes']={'before_large':old_digest,'native_large':native['probe']['estimates_sha256'],'duck_large':duck['probe']['estimates_sha256'],'before_real':real_digest,'native_real':native_real['probe']['estimates_sha256']}
    if len({old_digest,native['probe']['estimates_sha256'],duck['probe']['estimates_sha256']})!=1:raise RuntimeError('Large fitted results differ; inspect source IDs/grouping before interpreting performance')
    if real_digest!=native_real['probe']['estimates_sha256']:raise RuntimeError('Reference outputs changed')
    execute('prepared-map-verified',[run.BUILD/'mapbench-preparation.exe','-data',root/'native-large-data','-requests','200'])
    reopened=execute('prepared-reopen',[run.BUILD/'fullprep-after.exe','-data',root/'native-large-data','-workers','8','-solver','fast','-verify-existing'])
    if reopened['probe']['estimates_sha256']!=old_digest:raise RuntimeError('Reopen changed saved results')
    report['speedups']={'large_before_default_to_tuned_native':before['probe']['seconds']/native['probe']['seconds'],'real_matched_reference_two_workers':real_before['probe']['seconds']/native_real['probe']['seconds'],'duck_vs_native_wall_ratio':duck['probe']['seconds']/native['probe']['seconds']}
    run.write_json(root/'summary.json',report);run.write_json(run.ROOT/'benchmarks/baselines/preparation-performance.json',report)
    print(json.dumps(report['speedups']),flush=True)

if __name__=='__main__':main()
