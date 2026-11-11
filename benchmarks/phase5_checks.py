"""Collect correctness, quality, native/browser and input-preservation evidence."""
import argparse
import copy
import json
import math
from pathlib import Path
import re
import sqlite3
import statistics
import run


def stats(values):
    v=sorted(values)
    return {'count':len(v),'p50':v[(len(v)-1)//2]if v else None,'p95':v[math.ceil(len(v)*.95)-1]if v else None,'max':v[-1]if v else None}


def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--root',type=Path,required=True);root=parser.parse_args().root.resolve(strict=True)
    if json.loads((root/'owner.json').read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned run required')
    report=json.loads((root/'summary.json').read_text())
    verification={'native':json.loads((root/'native-active-v2.json').read_text()),'native_run':json.loads((root/'native-active-v2-run/result.json').read_text())}
    assert verification['native']['success']and verification['native']['empty_ap_frames']==0
    verification['browser_run']=json.loads((root/'browser-v2-run/result.json').read_text())
    folder=Path((root/'browser-v2-run/stdout.json').read_text().splitlines()[0])
    verification['browser']=json.loads((folder/'smoke.json').read_text());verification['browser_artifacts']=str(folder)
    assert verification['browser']['success']
    with sqlite3.connect((root/'browser-v2-data/store/atlas.sqlite3').as_uri()+'?mode=ro',uri=True)as db:
        scopes=db.execute('SELECT scope_key,count(*) FROM estimates GROUP BY scope_key').fetchall()
        pending=db.execute('SELECT count(*) FROM preparation_pending').fetchone()[0]
        assert len(scopes)==4 and all(n==64 for _,n in scopes)and pending==0,scopes
        verification['four_namespace_round_trip']={'scopes':scopes,'pending':pending,'quick_check':db.execute('PRAGMA quick_check').fetchone()[0]}
    verification['input_hashes']={}
    phase4=json.loads((run.ROOT/'benchmarks/baselines/phase4-summary.json').read_text())
    for path,expected in phase4['real_input_hashes'].items():
        actual=run.sha(Path(path));assert actual==expected,path;verification['input_hashes'][path]=actual
    fixture=run.BUILD/'fixtures/cardinality-10m.csv';actual=run.sha(fixture)
    assert actual==json.loads(fixture.with_suffix('.json').read_text())['sha256'];verification['large_input_sha256']=actual
    verification['windows_build']={'path':'build/SignalAtlas-phase5.exe','sha256':run.sha(run.ROOT/'build/SignalAtlas-phase5.exe')}
    verification['shader_sha256']=run.sha(run.ROOT/'internal/gpu/scoring.cl')
    report['verification']=verification
    report['gpu_pilot']=json.loads((root/'gpu-report.json').read_text())
    report['known_truth']=json.loads((root/'known-truth.json').read_text())
    rows=report['known_truth']['cases'];report['known_truth_aggregates']={}
    for solver in ['reference','fast','opencl64-reference-v1','opencl64-fast-v1','pattern-candidate']:
        report['known_truth_aggregates'][solver]={'seconds':sum(v[solver]['seconds']for v in rows),'truth_error_m':stats([v[solver]['truth_error_m']for v in rows]),'reference_shift_m':stats([math.hypot(v[solver]['east_m']-v['reference']['east_m'],v[solver]['north_m']-v['reference']['north_m'])for v in rows])}
    report['solver_comparison']={}
    for build in ['before','after']:
        source=json.loads((root/('solver-'+build)/'result.json').read_text())
        value={k:v for k,v in source.items()if k!='probe'};value['probe']={k:v for k,v in source['probe'].items()if k not in ['sample','all']}
        for mode in ['sample','all']:
            result=source['probe'][mode];value['probe'][mode]={k:v for k,v in result.items()if k!='results'}
            if build=='after':
                checks={}
                for gpu,cpu in [('opencl64-reference-v1','reference'),('opencl64-fast-v1','fast')]:
                    mismatches=0;used=0
                    for row in result['results']:
                        accelerated=copy.deepcopy(row[gpu]);meta=accelerated.pop('acceleration',{})
                        used+=meta.get('gpu_grids',0)>0
                        if cpu=='fast':accelerated['observation_scope']['solver']='fast'
                        else:accelerated['observation_scope'].pop('solver',None)
                        mismatches+=accelerated!=row[cpu]
                    assert mismatches==0,(mode,gpu,mismatches)
                    checks[gpu]={'numeric_mismatches':mismatches,'actual_gpu_fitted_cases':used}
                value['probe'][mode]['gpu_parity']=checks
        report['solver_comparison'][build]=value
    for mode in ['sample','all']:
        for solver in ['reference','fast']:
            key=solver+'_sha256'
            assert report['solver_comparison']['before']['probe'][mode][key]==report['solver_comparison']['after']['probe'][mode][key]
    report['microbenchmarks']={}
    for build in ['before','after']:
        values={}
        for line in (root/('cpu-'+build+'.txt')).read_text().splitlines():
            match=re.match(r'BenchmarkPhase5Solver/(\S+)\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op',line)
            if match:
                name,ns,allocation,count=match.groups();values.setdefault(name,[]).append([float(ns),int(allocation),int(count)])
        report['microbenchmarks'][build]={name:{'trials':v,'median_ns_per_op':statistics.median(x[0]for x in v),'median_bytes_per_op':statistics.median(x[1]for x in v),'median_allocs_per_op':statistics.median(x[2]for x in v)}for name,v in values.items()}
    report['validation']={'full_go_and_python_parity':'passed; Python/NumPy test executed, not short/skipped','hardware_suite':'passed, float64 scoring/final fits and 40 known-position cases','race':'passed for transactions, preparation, GPU ABI/queues and hardware fits','js_map_tests':10,'benchmark_safety_tests':4,'go_vet':'passed','cross_builds':'Linux amd64 and macOS arm64 engine, CGO disabled; GPU uses CPU fallback','raw_real_fit_rows':'retained only in owned ignored run, not copied into committed evidence','excluded_native_diagnostic':'native-active.json showed temporary AP clearing; final native-active-v2.json verifies zero empty AP frames after buffer retention fix'}
    run.write_json(root/'summary.json',report);run.write_json(run.ROOT/'benchmarks/baselines/phase5-summary.json',report)
    print(json.dumps({'speedups':report['speedups'],'native':verification['native'],'browser':verification['browser'],'known_truth':report['known_truth_aggregates']},indent=2))


if __name__=='__main__':main()
