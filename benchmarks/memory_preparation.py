"""Reproduce memory/preparation comparisons using fresh, explicitly owned copies.

--snapshot-source permits a read-only backup of an existing physical Atlas store.
Without it, the source must already be an owned benchmark fixture. Never imports,
cleans, pauses or changes the application's active survey.
"""
from contextlib import closing
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import tempfile

import phase2
import run


def snapshot_source(source, dest):
    source = source.resolve(strict=True)
    if dest.exists():
        raise ValueError('Snapshot destination must be new')
    manifest_path = source/'observations/manifest.json'
    before = manifest_path.read_bytes()
    manifest = json.loads(before)
    names = [x['file'] for x in manifest['segments']]
    if any(Path(n).name != n or '/' in n or chr(92) in n or not n.startswith('seg-') or not n.endswith('.bin') for n in names):
        raise ValueError('Unsafe segment path')
    dest.mkdir(parents=True)
    with closing(sqlite3.connect(source.joinpath('atlas.sqlite3').as_uri()+'?mode=ro', uri=True)) as original, closing(sqlite3.connect(dest/'atlas.sqlite3')) as copy:
        original.backup(copy)
        expected = sum(x['n'] for x in manifest['segments'])
        if copy.execute('SELECT coalesce(sum(active_rows),0) FROM imports').fetchone()[0] != expected:
            raise ValueError('Import in progress or manifest/metadata mismatch')
        if copy.execute('SELECT count(*) FROM observations').fetchone()[0] != 0:
            raise ValueError('Native column-store fixture required')
    (dest/'observations').mkdir()
    for name in names:
        try:
            os.link(source/'observations'/name, dest/'observations'/name)
        except OSError:
            shutil.copyfile(source/'observations'/name, dest/'observations'/name)
    for name in ['strings.bin','manifest.json']:
        shutil.copyfile(source/'observations'/name, dest/'observations'/name)
    if before != manifest_path.read_bytes():
        raise ValueError('Source changed during snapshot; do not benchmark this copy')
    return {'rows': expected, 'strings': manifest['strings'], 'segments': len(names),
            'manifest_sha256': hashlib.sha256(before).hexdigest()}


def fresh_fixture(source, dest, stride=1):
    """Keep all observations/IDs; remove derived results in the new copy only."""
    if stride < 1:
        raise ValueError('Positive cohort stride required')
    # Reuse the existing ownership/independent-copy guard before any mutation.
    phase2.clone_workspace(source, dest)
    path = dest/'atlas.sqlite3'
    with closing(sqlite3.connect(path)) as c:
        # Suspend copied triggers while clearing derived data in this owned copy,
        # then restore the exact definitions for either benchmark adapter.
        definitions=c.execute("SELECT name,sql FROM sqlite_master WHERE type='trigger'").fetchall()
        for name,_ in definitions:
            c.execute('DROP TRIGGER "'+name.replace('"','""')+'"')
        tables = {r[0] for r in c.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        for table in ['estimates','preparation_checkpoints','preparation_pending','preparation_namespaces','map_changes','map_locations','located_networks']:
            if table in tables:
                c.execute('DELETE FROM '+table)
        c.execute("DELETE FROM map_assets WHERE name='preparation_settings'")
        if stride > 1:
            c.execute('DELETE FROM preparation_targets WHERE rowid%?<>0', (stride,))
        for _,sql in definitions:
            c.execute(sql)
        c.commit()


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', type=Path, required=True)
    p.add_argument('--snapshot-source', action='store_true')
    p.add_argument('--native', type=Path, required=True)
    p.add_argument('--baseline', type=Path)
    p.add_argument('--duck-probe', type=Path)
    p.add_argument('--ordered-duck', type=Path)
    p.add_argument('--stride', type=int, default=15, help='1 completes every target; all scans are retained')
    p.add_argument('--map-poll-ms', type=int, default=400)
    args=p.parse_args()
    root=Path(tempfile.mkdtemp(prefix='memory-preparation-',dir=run.BUILD))
    run.write_json(root/'owner.json',{'purpose':'signal-atlas-benchmark','schema':1})
    source=args.source.resolve(strict=True)
    report={'schema':1,'root':str(root),'cohort_stride':args.stride,'all_scans_retained':True}
    if args.snapshot_source:
        report['input']=snapshot_source(source,root/'snapshot/store')
        source=root/'snapshot/store'
    fresh_fixture(source,root/'fresh/store',args.stride)
    report['runs']={}
    for label,exe in [('baseline',args.baseline),('native',args.native)]:
        if exe is None:
            continue
        data=root/(label+'-data')
        phase2.clone_workspace(root/'fresh/store',data/'store')
        command=[str(exe.resolve(strict=True)),'-data',str(data),'-workers','8','-solver','fast','-gpu']
        if args.map_poll_ms:
            command+=['-map-poll-ms',str(args.map_poll_ms)]
        report['runs'][label]=run.execute(command,root/label,timeout=1800,max_rss_bytes=8<<30)
    if args.duck_probe:
        if not args.ordered_duck:
            p.error('--ordered-duck is required with --duck-probe')
        data=root/'duck-data'
        phase2.clone_workspace(root/'fresh/store',data/'store')
        command=[str(args.duck_probe.resolve(strict=True)),'-db',str(args.ordered_duck.resolve(strict=True)),'-ordered','-metadata',str(data/'store/atlas.sqlite3'),'-workers','8','-solver','fast','-gpu','-duck-threads','2','-memory','256MB','-batch','512']
        report['runs']['duck']=run.execute(command,root/'duck',timeout=1800,max_rss_bytes=4<<30)
    hashes={r['probe']['estimates_sha256'] for r in report['runs'].values() if 'probe' in r}
    report['matching_result_hashes']=len(hashes)==1
    run.write_json(root/'summary.json',report)
    if len(hashes)>1:
        raise RuntimeError('Saved-result mismatch')
    print(root/'summary.json')


if __name__=='__main__':
    main()
