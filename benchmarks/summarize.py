"""Summarize retained local phase-0 evidence without publishing survey records."""
import json
import math
from pathlib import Path
import statistics
import time
import run as bm

def read(path):return json.loads(Path(path).read_text(encoding='utf-8-sig'))
def gib(n):return round(n/1024**3,3)

groups={}
for path in sorted(bm.BUILD.glob('run-*/baseline.json')):
    r=read(path);trials=r['trials'];successful=[t for t in trials if 'probe' in t]
    if not successful:continue
    engine=successful[0]['probe']['engine'];fixture=r['environment']['fixture']
    stem=successful[0]['probe']['imports'][0]['file'].removesuffix('.csv')
    rates=[t['probe']['imports'][0]['rows_per_minute']/1e6 for t in successful]
    p95={};limited={}
    for t in successful:
        for endpoint,x in t['probe'].get('viewport_traces',{}).items():
            if x.get('count',0)>=200 and endpoint.startswith('/api/points?'):
                p95.setdefault(endpoint,[]).append(x['p95_ms'])
            elif 1<x.get('count',0)<200 and endpoint.startswith('/api/points?') and endpoint not in limited:
                limited[endpoint]={'count':x['count'],'median_ms':x['p50_ms'],'runs_ms':x['runs_ms'],'interpretation':'limited diagnostic; not a 200-request p95'}
    out={'engine':engine,'fixture':stem,'rows':fixture['rows'],'radios':fixture['radios'],'fixture_sha256':fixture['sha256'],'trials':len(trials),'completed_trials':len(successful),'import_million_rows_per_minute':{'median':statistics.median(rates),'min':min(rates),'max':max(rates)},'peak_engine_working_set_gib':max(gib(t['peak_engine_rss_bytes']) for t in trials),'process_cold_reopen_seconds':[t['cold_reopen']['probe']['open_seconds'] for t in successful if 'probe' in t.get('cold_reopen',{})],'warm_observation_p95_ms':{p:statistics.median(v) for p,v in p95.items()},'stopped_runs':[t['stopped_reason'] for t in trials if t.get('stopped_reason')],'raw_result':str(path.relative_to(bm.ROOT))}
    out['limited_observation_samples']=limited
    out['process_cold_first_summary_seconds']=[t['cold_reopen']['probe']['summary']['wall_seconds'] for t in successful if 'wall_seconds' in t.get('cold_reopen',{}).get('probe',{}).get('summary',{})]
    groups[(engine,stem)]=out

extras={}
for path in sorted(bm.BUILD.glob('extra-*/index.json'),key=lambda p:p.stat().st_mtime):extras.update(read(path))
reopen_queries={}
for name,path in extras.items():
    if name.startswith('cold-donor-'):
        p=read(path)['probe'];reopen_queries[name]={'open_seconds':p['open_seconds'],'first_summary_seconds':p['first_summary_seconds'],'open_plus_first_summary_seconds':p['open_seconds']+p['first_summary_seconds']}

native={};excluded=[]
for note in bm.BUILD.glob('run-*/trial-*/abort-note.json'):
    detail=read(note)
    excluded.append({'raw_result':str(note.parent.relative_to(bm.ROOT)),'reason':{k:detail[k] for k in ['reason','created','stopped_utc','cpu_seconds','peak_working_set_bytes']}})
for path in sorted(bm.BUILD.glob('native-*/result.json'),key=lambda p:p.stat().st_mtime):
    r=read(path);n=r.get('native',{});errors=list(n.get('errors',[]))
    if r.get('stopped_reason'):errors.append(r['stopped_reason'])
    renderer=n.get('renderer','unknown')
    if renderer=='upstream MapLibre/deck.gl':
        for s in n.get('scenarios',[]):
            if s.get('observations_enabled'):
                total=s.get('total_observations',0);limit=s['point_limit'];expected=total//max(1,math.ceil(total/limit))
                if abs(expected-s.get('shown_observations',0))>32:errors.append('point count does not match scenario limit')
    if errors:
        excluded.append({'raw_result':str(path.relative_to(bm.ROOT)),'reason':errors});continue
    native[renderer]={'renderer':renderer,'peak_family_working_set_gib':gib(r.get('peak_family_working_set_bytes',0)),'peak_webview_children_working_set_gib':gib(r.get('peak_webview_children_working_set_bytes',0)),'viewport':n.get('viewport'),'webgl':n.get('webgl'),'scenarios':[{k:v for k,v in s.items() if k not in ('pan_frames','draw_cpu','draw_submit')}|{'pan_frame_p95_ms':s.get('pan_frames',{}).get('p95_ms'),'draw_cpu_p95_ms':s.get('draw_cpu',{}).get('p95_ms')} for s in n.get('scenarios',[])],'raw_result':str(path.relative_to(bm.ROOT))}

tests={}
for name in ['upstream','donor']:
    events=[json.loads(line) for line in (bm.BUILD/(name+'-tests.jsonl')).read_text(encoding='utf-8-sig').splitlines() if line.strip()]
    tests[name]={'passed':sum(e['Action']=='pass' and bool(e.get('Test')) for e in events),'failed':sum(e['Action']=='fail' for e in events),'skipped_tests':sum(e['Action']=='skip' and bool(e.get('Test')) for e in events)}
tests['exits']=read(bm.BUILD/'test-exits.json');tests['python_reference_tests']=62;tests['harness_safety_tests']=4
tests['donor']['expected_helper_skip']='TestDatabaseFileLockHelper is deliberately skipped outside its parent subprocess test; TestDatabaseLockedByAnotherProcessKeepsCurrentStore passed.'
tests['harness_go_safety_tests']=1

special={}
for name in ['real-inputs','incremental','archive-1m','dense-ap','export-10m','default-10m','dense-profile']:
    if name not in extras:continue
    r=read(extras[name]);p=r.get('probe',{})
    if name=='real-inputs':
        oracle=read(bm.BUILD/'real-inputs.json')
        special[name]={'accepted_wifi_rows':p['observation_count'],'import_seconds':sum(x['wall_seconds'] for x in p['imports']),'zero_coordinate_rows':sum(x['zero_coordinate_rows'] for x in oracle['files']),'future_filtered_wifi_rows':p['observation_count']-sum(x['zero_coordinate_rows'] for x in oracle['files']),'fit_batches':{k:{a:v[a] for a in ['attempted','fitted','unavailable','wall_seconds']} for k,v in p['fits'].items()},'raw_result':str(Path(extras[name]).relative_to(bm.ROOT))}
    elif name=='incremental':
        special[name]={k:v for k,v in p.items() if k!='concurrent_viewport_ms'}
        special[name]['active_cancel_measured']=p.get('before_cancel',{}).get('status')=='running' and p.get('cancelled',{}).get('status')=='cancelled'
    elif name=='archive-1m':
        special[name]={k:v for k,v in p.items() if k.startswith('archive_') and k not in ['archive_info','archive_export_days']}
    elif name=='dense-ap':
        special[name]={'traces':{k:{a:v[a] for a in ['count','p50_ms','p95_ms','max_ms','response_bytes_max']} for k,v in p['viewport_traces'].items()},'raw_result':str(Path(extras[name]).relative_to(bm.ROOT))}
    elif name=='export-10m':
        special[name]={'export_seconds':p['export_sqlite']['wall_seconds'],'snapshot_import_seconds':p['open_sqlite_snapshot']['wall_seconds'],'snapshot_rows':p['open_sqlite_snapshot']['result']['valid_rows'],'peak_engine_working_set_gib':gib(r['peak_engine_rss_bytes']),'raw_result':str(Path(extras[name]).relative_to(bm.ROOT))}
    elif name=='default-10m':
        special[name]={'threads':p['threads'],'import':p['imports'][0],'peak_engine_working_set_gib':gib(r['peak_engine_rss_bytes'])}

required=[(e,f) for e in ['upstream','donor-duckdb'] for f in ['repeated-10m','cardinality-10m','capacity-50m']]
complete=all(k in groups for k in required) and len(native)==3 and special.get('incremental',{}).get('active_cancel_measured',False) and all(x==0 for x in tests['exits'].values())
summary={'schema':1,'recorded_utc':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),'upstream_commit':'6d8833659906faa89b1677424caf5f5cd905c56d','phase0_status':'complete' if complete else 'incomplete','host':{'cpu':'Intel Core i7-10700K','physical_cores':8,'logical_cpus':16,'usable_ram_bytes':34232709120,'display_adapter':'NVIDIA GeForce RTX 2080 Ti','storage':'Intel Raid 0 Volume','controlled_gomaxprocs':8,'duckdb_working_memory':'2GB','engine_working_set_ceiling_gib':12,'native_family_working_set_ceiling_gib':6},'imports':list(groups.values()),'special':special,'native':list(native.values()),'excluded_diagnostic_runs':excluded,'tests':tests,'sources':read(bm.ROOT/'benchmarks'/'baselines'/'sources.json')}
summary['fresh_process_donor_open_and_first_query']=reopen_queries
# Source manifests contain source files only. Real records and fit identifiers
# stay in ignored raw evidence; this summary contains aggregate measurements.
bm.write_json(bm.ROOT/'benchmarks'/'baselines'/'phase0-summary.json',summary)
print(json.dumps({'status':summary['phase0_status'],'import_cases':len(groups),'native_cases':len(native),'active_cancel':special.get('incremental',{}).get('active_cancel_measured'),'tests':tests},indent=2))
