"""Publish the source-safe Phase 4 evidence, with raw results and sample counts."""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import statistics
import run

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,required=True)
    args=parser.parse_args();root=args.root.resolve(strict=True)
    def result(name):
        value=json.loads((root/name/'result.json').read_text(encoding='utf-8'))
        if value['exit_code'] or value.get('stopped_reason'):raise RuntimeError('Failed probe: '+name)
        return value
    report={'schema':1,'recorded_utc':datetime.now(timezone.utc).isoformat(),'root':str(root),
            'base_commit':'481057a','upstream_commit':'6d8833659906faa89b1677424caf5f5cd905c56d',
            'decision':'Retain optimized observation columns and SQLite metadata; use real-observation read-only SQL adapter. DuckDB remains benchmark-only.',
            'threads':8,'duckdb_memory_limit':'1GB','cache_policy':'Filesystem cache uncontrolled; reopen is a new process, not cold disk.',
            'workloads':{},'checks':{},'excluded_intermediate_runs':[]}
    for profile in ['cardinality-10m','repeated-10m']:
        manifest=json.loads((run.BUILD/'fixtures'/f'{profile}.json').read_text())
        entry={'input':manifest,'engines':{}}
        for engine in ['before','final','duckdb']:
            trials=[]
            for trial in range(1,4):
                name=f'{engine}-{profile}'+(''if trial==1 else f'-{trial}')
                item=result(name);probe=item['probe'];imp=probe['import_'+profile+'.csv']if engine=='duckdb'else probe['imports'][0]
                if imp['outcome']['valid_rows']!=9_999_900 or imp['outcome']['zero_excluded_rows']!=100:raise RuntimeError('Count mismatch: '+name)
                trials.append(item)
            def imported(item):return item['probe']['import_'+profile+'.csv']if engine=='duckdb'else item['probe']['imports'][0]
            entry['engines'][engine]={'import_seconds_median':statistics.median(imported(t)['wall_seconds']for t in trials),
                'accepted_rows_per_minute_median':statistics.median(imported(t)['rows_per_minute']for t in trials),
                'peak_working_set_bytes_median':statistics.median(t['peak_engine_rss_bytes']for t in trials),
                'trials':trials,'reopen':result(f'{engine}-{profile}-reopen')}
        report['workloads'][profile]=entry
        for engine in ['after','optimized']:
            for name in [f'{engine}-{profile}',f'{engine}-{profile}-2',f'{engine}-{profile}-3',f'{engine}-{profile}-reopen']:
                p=root/name/'result.json'
                if p.exists():
                    d=json.loads(p.read_text());report['excluded_intermediate_runs'].append({'path':str(p),'probe_sha256':d.get('probe_sha256'),'reason':'Superseded by final implementation; excluded from headline medians.'})
    for name in ['sql-cardinality-10m','sql-repeated-10m','duckdb-indexed-cardinality','map','capacity-column','capacity-duckdb','real-imports','incremental-column','incremental-duckdb','native-map','browser-sql-final','native-sql-final']:
        if (root/name/'result.json').exists():report['checks'][name]=result(name)
    report['checks']['capacity-column']['source_stage']='Bounded pipeline before append-only optimization; the capacity import uses the unchanged full-index branch.'
    report['checks']['map']['source_stage']='Full-state read/render path; append-only indexing is not exercised.'
    for name in ['capacity-column','capacity-duckdb']:
        p=report['checks'][name]['probe'];count=p.get('rows',p.get('observation_count'))
        if count!=49_999_500:raise RuntimeError('Capacity count mismatch')
    report['append']=json.loads((root/'append-summary.json').read_text())
    report['append']['median_seconds']=statistics.median(t['probe']['incremental_import']['wall_seconds']for t in report['append']['trials'])
    if 'incremental-column'in report['checks']:report['append']['earlier_full_rebuild_seconds']=report['checks']['incremental-column']['probe']['incremental_import']['wall_seconds']
    native_final=(root/'final-sql/native-sql.json').exists()
    native_name='native-sql-final'if native_final else 'native-sql'
    if native_name not in report['checks']:report['checks'][native_name]=result(native_name)
    native=json.loads((root/('final-sql/native-sql.json'if native_final else 'native-sql.json')).read_text())
    if not native.get('success'):raise RuntimeError('Native SQL UI failed')
    report['checks'][native_name]['ui_result']=native
    native_map=json.loads((root/'native-map.json').read_text())
    if native_map.get('errors'):raise RuntimeError('Native map UI failed')
    report['checks']['native-map']['ui_result']=native_map
    browser_name='browser-sql-final'if (root/'browser-sql-final/result.json').exists()else 'browser-sql'
    if browser_name not in report['checks']:report['checks'][browser_name]=result(browser_name)
    browser_folder=Path((root/browser_name/'stdout.json').read_text().splitlines()[0])
    browser=json.loads((browser_folder/'smoke.json').read_text())
    if not browser.get('success'):raise RuntimeError('Browser SQL UI failed')
    report['checks'][browser_name]['ui_result']=browser
    report['checks'][browser_name]['artifact_folder']=str(browser_folder)
    additional=json.loads((root/'checks-summary.json').read_text())
    report['real_input_hashes']=additional['real_input_hashes']
    report['tests']={
        'full_go_suite_before_append':'passed, including Python/NumPy parity',
        'after_append':['all internal/obs, internal/atlas and internal/engine tests passed','Python/NumPy parity passed','go vet ./... passed','race detector passed for append/snapshot/import/SQL lifecycle paths'],
        'javascript':'eight map-display tests, SQL script syntax, browser/native SQL checks passed',
        'benchmark_safety':'four safety tests passed'}
    destination=run.ROOT/'benchmarks/baselines/phase4-summary.json';run.write_json(destination,report);print(destination)

if __name__=='__main__':main()
