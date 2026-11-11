"""Post-sweep integrity, input preservation and real desktop/browser checks."""
import argparse
import json
from pathlib import Path
import sqlite3
import run
from phase2 import clone_workspace


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,required=True)
    root=parser.parse_args().root.resolve(strict=True)
    if json.loads((root/'owner.json').read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:
        raise ValueError('Owned benchmark root required')
    summary=json.loads((root/'summary.json').read_text())
    report={'schema':1,'integrity':{}}
    for name,relative in [('before_large','before-fast-data/store/atlas.sqlite3'),('native_large','native-large-data/store/atlas.sqlite3'),('duck_large','duck-metadata/preparation.sqlite3'),('before_real','before-real-data/store/atlas.sqlite3'),('native_real','native-real-data/store/atlas.sqlite3')]:
        with sqlite3.connect((root/relative).as_uri()+'?mode=ro',uri=True) as db:
            check=db.execute('PRAGMA quick_check').fetchall()
            pending=db.execute('SELECT count(*) FROM preparation_pending').fetchone()[0]
            stale=db.execute('SELECT count(*) FROM preparation_checkpoints c JOIN preparation_targets t USING(target_key) WHERE c.version<>t.version').fetchone()[0]
            assert check==[('ok',)] and pending==0 and stale==0,(name,check,pending,stale)
            report['integrity'][name]={'quick_check':'ok','pending':pending,'stale_checkpoints':stale}
    phase4=json.loads((run.ROOT/'benchmarks/baselines/phase4-summary.json').read_text())
    report['real_input_hashes']={name:run.sha(Path(name)) for name in phase4['real_input_hashes']}
    assert report['real_input_hashes']==phase4['real_input_hashes'],'Original scan input changed'
    fixture=run.BUILD/'fixtures/cardinality-10m.csv'
    report['large_input_sha256']=run.sha(fixture)
    assert report['large_input_sha256']==json.loads(fixture.with_suffix('.json').read_text())['sha256']
    small=run.BUILD/'phase3-g_ifubvu/small-data/store'
    for name in ['desktop','browser']:
        dest=root/(name+'-check-data')/'store'
        if not dest.exists():clone_workspace(small,dest)
        with sqlite3.connect(dest/'atlas.sqlite3') as db:
            assert db.execute('SELECT count(*) FROM preparation_checkpoints').fetchone()[0]==0,'Fresh UI check fixture required'
            db.execute("INSERT OR REPLACE INTO map_assets VALUES('preparation_settings',?)",(json.dumps({'version':1,'enabled':True,'paused':True,'fit_mode':'sample','grouping':'shared','workers':2,'memory_mb':256}),))
    report['desktop_run']=run.execute([str(run.BUILD/'nativebench-preparation.exe'),'-repo','.', '-data',str(root/'desktop-check-data'),'-page','gpu','-automatic','-driver','phase3-native.js','-output',str(root/'desktop-check.json')],root/'desktop-check-run',timeout=180,max_rss_bytes=6*1024**3,family=True,json_output=False)
    report['desktop']=json.loads((root/'desktop-check.json').read_text())
    assert report['desktop']['success'],report['desktop']
    report['browser_run']=run.execute(['python','benchmarks/visual.py','--data',str(root/'browser-check-data'),'--app','build/SignalAtlas-preparation.exe','--driver','preparation-smoke.cjs'],root/'browser-check-run',timeout=180,max_rss_bytes=6*1024**3,family=True,json_output=False)
    folder=Path((root/'browser-check-run/stdout.json').read_text().splitlines()[0])
    report['browser']=json.loads((folder/'smoke.json').read_text())
    assert report['browser']['success'],report['browser']
    report['browser_artifacts']=str(folder)
    report['windows_build']={'path':'build/SignalAtlas-preparation.exe','sha256':run.sha(run.ROOT/'build/SignalAtlas-preparation.exe')}
    summary['verification']=report
    run.write_json(root/'verification.json',report)
    run.write_json(root/'summary.json',summary)
    run.write_json(run.ROOT/'benchmarks/baselines/preparation-performance.json',summary)
    print(json.dumps(report,indent=2))


if __name__=='__main__':main()
