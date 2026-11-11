"""Phase 2 probes: never open the source fixture or modify input logs."""
import argparse
import json
import os
from pathlib import Path
import shutil
import sqlite3
import tempfile

import run


def clone_workspace(source, dest):
    """Independent metadata/dictionary; immutable segments may share disk blocks."""
    source = source.resolve(strict=True)
    if not any((p / 'owner.json').is_file() and json.loads((p / 'owner.json').read_text()).get('purpose') == 'signal-atlas-benchmark'
               for p in [source, *list(source.parents)[:4]]):
        raise ValueError('Source must be an owned benchmark fixture')
    dest.mkdir(parents=True, exist_ok=False)
    with sqlite3.connect(source.joinpath('atlas.sqlite3').as_uri()+'?mode=ro', uri=True) as original, sqlite3.connect(dest/'atlas.sqlite3') as copy:
        original.backup(copy)
    (dest/'observations').mkdir()
    for file in (source/'observations').iterdir():
        if file.name.startswith('seg-') and file.suffix == '.bin':
            try:
                os.link(file, dest/'observations'/file.name)
                continue
            except OSError:
                pass
        if file.name in ('manifest.json','strings.bin') or file.name.startswith('seg-'):
            shutil.copyfile(file, dest/'observations'/file.name)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--legacy', default='build/benchmarks/run-hs077f3e/trial-1/store/store')
    parser.add_argument('--small', default='build/benchmarks/run-81xmpu1_/trial-1/store/store')
    parser.add_argument('--csv', default='build/benchmarks/fixtures/cardinality-10m.csv')
    args=parser.parse_args()
    root=Path(tempfile.mkdtemp(prefix='phase2-',dir=run.BUILD))
    run.write_json(root/'owner.json',{'purpose':'signal-atlas-benchmark','schema':1})
    clone_workspace(Path(args.legacy),root/'admin-data'/'store')
    clone_workspace(Path(args.small),root/'ui-data'/'store')
    source=Path(args.csv).resolve(strict=True)
    before=run.sha(source)
    report={'schema':1,'root':str(root),'input_sha256':before}
    report['administration']=run.execute([str(run.BUILD/'adminbench-phase2.exe'),'-data',str(root/'admin-data')],root/'administration')
    report['import_10m']=run.execute([str(run.BUILD/'baseline-phase2.exe'),'-csv',str(source),'-store',str(root/'import-data'),'-expected-rows','9999900','-threads','8','-fits','0','-requests','5','-map-only'],root/'import-10m')
    if run.sha(source)!=before: raise RuntimeError('Input file changed')
    inputs=sorted((run.ROOT.parent/'inputs').glob('*.csv'))
    if inputs:
        hashes={str(p):run.sha(p) for p in inputs}
        report['real_inputs']=run.execute([str(run.BUILD/'baseline-phase2.exe'),'-csv',';'.join(map(str,inputs)),'-store',str(root/'real-data'),'-expected-rows','175447','-threads','8','-fits','0','-requests','5','-map-only'],root/'real-imports')
        if hashes!={str(p):run.sha(p) for p in inputs}: raise RuntimeError('Original input file changed')
        report['real_input_hashes']=hashes
    run.write_json(root/'summary.json',report)
    print(root)


if __name__=='__main__':
    main()
