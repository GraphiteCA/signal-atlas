"""Collect sanitized evidence; private AP identities and query plans stay local."""
from contextlib import closing
import argparse
import hashlib
import json
from pathlib import Path
import sqlite3

import run
from phase5 import normalized_digest


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--root',type=Path,required=True);args=p.parse_args();root=args.root.resolve(strict=True)
    if json.loads((root/'owner.json').read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned run required')
    def read(name):return json.loads((root/name).read_text(encoding='utf-8'))
    def compact(name):
        raw=read(name+'/result.json')
        out={k:raw[k] for k in ['exit_code','process_wall_seconds','peak_engine_rss_bytes','rss_sample_interval_ms','stopped_reason','probe_sha256']}
        if raw['exit_code']!=0 or raw['stopped_reason']:raise ValueError('Incomplete primary run: '+name)
        v=raw['probe'];out['probe']={k:v[k] for k in ['seconds','rows','solver','workers','threads','duckdb_threads','memory_limit','read_batch_targets','stats','saved','targets_per_second','fitted_per_second','read_seconds','read_batches','compute_seconds','commit_seconds','commit_batches','estimates_sha256','acceleration','live_map','streamed_target_catalog','cache_durability','gpu_requested','verification_only'] if k in v}
        if 'metrics' in v:out['probe']['metrics']={k:v['metrics'][k] for k in ['compute_seconds','commit_seconds','commit_batches','targets_committed','locations_written'] if k in v['metrics']}
        if v.get('samples'):out['probe']['first_completed_seconds']=next((s['seconds'] for s in v['samples'] if s.get('completed',0)>0),None)
        return out
    report={'schema':1,'base_commit':'0b73cab','branch':'codex/memory-and-preparation','private_records_committed':False,
        'measurement':'User paused active preparation before quiet/release comparisons. App remains open. Process-cold; filesystem cache uncontrolled. Individual full sweeps, not replicated latency distributions.',
        'machine':{'cpu':'i7-10700K','physical_cores':8,'logical_cores':16,'ram_gib':32,'gpu':'RTX 2080 Ti','driver':'621.01','go':'1.27.3','duckdb':'2.0.0','duckdb_go':'v2.20000.0'},
        'input':{k:v for k,v in read('snapshot.json').items() if k not in ['source','note']},'runs':{}}
    names=['quiet-baseline-live','release-live400','release-live50','full-native','full-duck','release-full-native','release-full-duck','release-duck-cohort','release-reopen']
    for name in names:report['runs'][name]=compact(name)
    full_hash='ebe35e7920bce842a4119d49032416f26e07c289ffad6170ba2924413b7aa914';cohort_hash='0acf515409e7747b9263da653cc47ef01a8101ac0e38909a306c5dfef303af2f'
    for name,v in report['runs'].items():
        data=v['probe'];is_full=name in ['full-native','full-duck','release-full-native','release-full-duck','release-reopen']
        assert data['rows']==5753026 and data['estimates_sha256']==(full_hash if is_full else cohort_hash),name
        assert data['stats']==({'completed':1460561,'fitted':170765,'unavailable':1289796} if is_full else {'completed':97370,'fitted':11299,'unavailable':86071}),name
    report['sampler']={}
    for name in ['sampler-baseline','sampler-release']:
        r=read(name+'/result.json');v=r['probe'];report['sampler'][name]={k:v[k] for k in ['rows','iterations','seconds','ms_per_fit','allocated_bytes_per_fit','result_sha256']}
    assert report['sampler']['sampler-baseline']['result_sha256']==report['sampler']['sampler-release']['result_sha256']
    report['memory_diagnostic']={}
    for name in ['memory-baseline2','memory-final']:
        r=read(name+'/result.json');v=r['probe'];report['memory_diagnostic'][name]={'peak_engine_rss_bytes':r['peak_engine_rss_bytes'],'stages':{k:v[k] for k in ['open','open_live','map','map_live']},'uncached_summary_ms':v['uncached_summary_ms']}
    native=read('native-release-v2.json');native.pop('status_text',None);assert native['success'] and native['empty_ap_frames']==0 and native['loading_frames']==0
    r=read('native-release-v2-run/result.json');report['native']={'report':native,'run':{k:r[k] for k in ['exit_code','process_wall_seconds','peak_engine_rss_bytes','peak_family_working_set_bytes','peak_webview_children_working_set_bytes','probe_sha256']},'scene':'Initially 479407 sightings across survey; trace anchors an actual AP at z15, with 317 local sightings. Dense synthetic rendering remains covered by Phase 5.'}
    inspect=read('duck-inspection/stdout.json');assert inspect['ordered_fingerprint']==inspect['canonical_fingerprint']
    report['duck_inspection']={k:inspect[k] for k in ['requests','rows_returned','point_ms','ordered_fingerprint','canonical_fingerprint']}
    report['duck_inspection']['point_state']='Target-aware warm lookups of all 17 original columns; identity query precedes each timed read. Two threads, 512 MB buffer limit. Not a comparison against a newly measured native radio-query trace.'
    report['duck_inspection']['legacy_plan_has_blockwise_join']='blockwise' in inspect['unordered_plan'].lower()
    report['duck_inspection']['ordered_plan_has_target_filter']='target_key' in inspect['ordered_plan'].lower()
    report['cpu_gpu_numeric_parity']={'normalization':'Only GPU provenance and solver token removed; numeric/identity/metadata fields retained.'}
    cpu=normalized_digest(root/'quiet-final-cpu-data/store/atlas.sqlite3');gpu=normalized_digest(root/'quiet-final-threads16-data/store/atlas.sqlite3');assert cpu==gpu
    report['cpu_gpu_numeric_parity']['normalized_sha256']=cpu
    report['exploratory']={'worker_runs':'after4-gpu-w4/w8/w16 were before user pause; not primary timing evidence. Quiet CPU/GPU/core runs use the previous save policy.','unordered':'Stopped the verified owned process before its first batch completed; no complete timing or result hash claimed.','native_pilot':'Excluded native-release.json: fixed synthetic coordinates were nearly empty in this survey. Replaced by actual-AP anchored native-release-v2.json.'}
    report['sqlite_integrity']={}
    for label in ['release-full-native','release-full-duck']:
        with closing(sqlite3.connect((root/(label+'-data/store/atlas.sqlite3')).as_uri()+'?mode=ro',uri=True)) as c:
            check=c.execute('PRAGMA quick_check').fetchone()[0];pending=c.execute('SELECT count(*) FROM preparation_pending').fetchone()[0];stale=c.execute('SELECT count(*) FROM preparation_checkpoints p LEFT JOIN preparation_targets t USING(target_key) WHERE t.target_key IS NULL OR t.version<>p.version').fetchone()[0]
            assert check=='ok' and pending==stale==0
            report['sqlite_integrity'][label]={'quick_check':check,'pending':pending,'stale_checkpoints':stale}
    snapshot=read('snapshot.json');source=Path(snapshot['source']);manifest=source/'observations/manifest.json';copy=root/'snapshot-data/store/observations'
    assert run.sha(manifest)==snapshot['manifest_sha256'];assert run.sha(source/'observations/strings.bin')==run.sha(copy/'strings.bin')
    entries=json.loads(manifest.read_text())['segments'];old_time=(root/'snapshot.json').stat().st_mtime_ns
    assert all((source/'observations'/e['file']).stat().st_mtime_ns<=old_time for e in entries)
    report['source_audit']={'metadata_access':'read-only SQLite backup only; active pause was performed by the user','manifest_sha256_unchanged':run.sha(manifest),'dictionary_sha256_matches_independent_snapshot':run.sha(copy/'strings.bin'),'all_segment_mtimes_predate_snapshot':True,'segment_count':len(entries)}
    report['verification']={'go_test':'go test ./... passed, including active Python numerical parity (93.552 s)','go_vet':'passed','race':'obs, atlas and engine preparation/map/dictionary tests passed','javascript':'11 controller/display tests passed','benchmark_safety':'7 Python tests passed','duckdb_module':'go test ./... passed','cross_builds':['Linux amd64 core (CPU fallback)','macOS arm64 core (CPU fallback)'],'desktop':{'path':'build/SignalAtlas-memory.exe','sha256':run.sha(run.ROOT/'build/SignalAtlas-memory.exe')}}
    run.write_json(root/'sanitized-summary.json',report);run.write_json(run.ROOT/'benchmarks/baselines/memory-preparation-summary.json',report)
    print('Sanitized summary verified and written.')

if __name__=='__main__':main()
