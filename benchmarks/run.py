"""Reproducible local benchmarks. Uses only the Python standard library.

All generated data and reports live under build/benchmarks. No command deletes
an existing directory. Source manifests include untracked donor source files.
"""
from __future__ import annotations

import argparse
import ctypes
from ctypes import wintypes
from datetime import datetime, timedelta
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
BUILD = ROOT / "build" / "benchmarks"
SOURCE_DIRS = {"cmd", "internal", "templates", "scripts", "tests", "ui", "web", "python", "test", "packaging", "benchmarks"}
SOURCE_SUFFIXES = {".go", ".mod", ".sum", ".py", ".js", ".cjs", ".html", ".css", ".json", ".ps1", ".txt", ".md", ".yml", ".yaml", ".svg", ".png", ".ico", ".icns", ".woff", ".woff2", ".ttf", ".sh", ".nsi", ".manifest", ".desktop", ".rc", ".plist"}


def sha(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as f:
        for chunk in iter(lambda: f.read(4 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()


def freeze(repo, destination, donor=False):
    repo, destination = Path(repo).resolve(), Path(destination).resolve()
    if destination.exists():
        raise ValueError("source freeze destination must be new")
    # An explicit source allowlist includes intended untracked Go/UI files and
    # excludes inputs, database files, bundles, outputs, local config and secrets.
    candidates = git(repo, "ls-files", "--cached", "--others", "--exclude-standard", "-z").split("\0")
    files = []
    for name in sorted(set(candidates)):
        rel = Path(name)
        source = repo / rel
        if not name or not source.is_file() or source.is_symlink():
            continue
        if rel.suffix.lower() not in SOURCE_SUFFIXES and rel.name not in {"VERSION", "LICENSE", "NOTICE"}:
            continue
        if len(rel.parts) > 1 and rel.parts[0] not in SOURCE_DIRS:
            continue
        if any(p in {"__pycache__", "outputs", "inputs", "build", ".venv"} for p in rel.parts):
            continue
        if rel.parts[:2] == ('benchmarks', 'baselines'):
            continue
        if any(x in rel.name.lower() for x in ("secret", "credential", ".env")):
            continue
        files.append({"path": rel.as_posix(), "sha256": sha(source), "bytes": source.stat().st_size})
    digest = hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest()
    destination.mkdir(parents=True)
    for entry in files:
        target = destination / entry["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(repo / entry["path"], target)
    manifest = {"schema": 1, "head": git(repo, "rev-parse", "HEAD"), "source_sha256": digest,
                "includes_untracked": True, "role": "donor" if donor else "upstream", "files": files}
    write_json(destination / "source-manifest.json", manifest)
    return manifest


def generate(path, rows, radios, repeated, seed=20261110):
    """Deterministic WiGLE rows; each row has a unique capture timestamp.

    Exactly one in 100,000 rows is 0,0. Equator and prime-meridian controls
    remain valid. Rows are never sampled from private survey inputs.
    """
    path = Path(path)
    if path.exists():
        raise ValueError("fixture already exists; refusing to overwrite")
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8", newline="", buffering=4 << 20) as f:
        f.write("WiGLEWifi-1.4,appRelease=baseline,model=synthetic\n")
        f.write("MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type,Node\n")
        batch = []
        stamp = ""
        for i in range(rows):
            radio = (i + seed) % radios
            pos = (i // radios) % (32 if repeated else 20000)
            lat = 48.4 + (pos % 200) * .00001
            lon = -123.4 + ((pos * 17) % 200) * .00001
            if i % 100000 == 0:
                lat, lon = 0., 0.
            elif i % 100000 == 1:
                lat = 0.
            elif i % 100000 == 2:
                lon = 0.
            mac = f"02:{radio >> 24 & 255:02X}:{radio >> 16 & 255:02X}:{radio >> 8 & 255:02X}:{radio & 255:02X}:01"
            # One timestamp per scan, one capture per radio per timestamp.
            # This avoids artificially interning millions of unique timestamps
            # in the repeated-position fixture. High cardinality varies radios.
            if i % radios == 0:
                stamp = (datetime(2026, 10, 1, 12) + timedelta(seconds=i // radios)).strftime("%Y-%m-%d %H:%M:%S")
            ssid = "" if radio % 97 == 0 else f"net-{radio}"
            band, channel = (2412, 1) if radio % 2 == 0 else (5180, 36)
            batch.append(f"{mac},{ssid},[WPA2],{stamp},{channel},{band},{-40 - (i * 7) % 55},{lat:.6f},{lon:.6f},3,WIFI,node-{radio % 3}\n")
            if len(batch) == 10000:
                f.writelines(batch)
                batch.clear()
        f.writelines(batch)
    result = {"schema": 1, "generator": 2, "seed": seed, "rows": rows, "radios": radios,
              "repeated_positions": repeated, "sha256": sha(path), "bytes": path.stat().st_size,
              "zero_coordinate_rows": (rows + 99999) // 100000,
              "baseline_expected_rows": rows, "filtered_expected_rows": rows - (rows + 99999) // 100000}
    write_json(path.with_suffix(".json"), result)
    return result


def process_memory(pid, peak=True):
    """Peak working set of the actual engine process, including native memory."""
    if os.name == "nt":
        class Counters(ctypes.Structure):
            _fields_ = [("cb", wintypes.DWORD), ("faults", wintypes.DWORD)] + [(n, ctypes.c_size_t) for n in (
                "peak", "working", "quota_peak_paged", "quota_paged", "quota_peak_nonpaged", "quota_nonpaged", "pagefile", "peak_pagefile")]
        kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
        kernel.OpenProcess.restype = wintypes.HANDLE
        kernel.CloseHandle.argtypes = [wintypes.HANDLE]
        psapi = ctypes.WinDLL("psapi", use_last_error=True)
        psapi.GetProcessMemoryInfo.argtypes = [wintypes.HANDLE, ctypes.POINTER(Counters), wintypes.DWORD]
        handle = kernel.OpenProcess(0x0400 | 0x0010, False, pid)
        if not handle:
            return None
        try:
            c = Counters()
            c.cb = ctypes.sizeof(c)
            if psapi.GetProcessMemoryInfo(handle, ctypes.byref(c), c.cb):
                return c.peak if peak else c.working
        finally:
            kernel.CloseHandle(handle)
    elif sys.platform.startswith("linux"):
        try:
            for line in Path(f"/proc/{pid}/status").read_text().splitlines():
                if line.startswith("VmHWM:"):
                    return int(line.split()[1]) * 1024
        except (FileNotFoundError, ProcessLookupError):
            pass
    return None


def descendants(root_pid):
    if os.name != 'nt':
        return {root_pid}
    class Entry(ctypes.Structure):
        _fields_=[('size',wintypes.DWORD),('usage',wintypes.DWORD),('pid',wintypes.DWORD),('heap',ctypes.c_size_t),('module',wintypes.DWORD),('threads',wintypes.DWORD),('parent',wintypes.DWORD),('priority',wintypes.LONG),('flags',wintypes.DWORD),('exe',wintypes.WCHAR*260)]
    k=ctypes.WinDLL('kernel32',use_last_error=True)
    k.CreateToolhelp32Snapshot.argtypes=[wintypes.DWORD,wintypes.DWORD];k.CreateToolhelp32Snapshot.restype=wintypes.HANDLE
    k.Process32FirstW.argtypes=[wintypes.HANDLE,ctypes.POINTER(Entry)];k.Process32NextW.argtypes=[wintypes.HANDLE,ctypes.POINTER(Entry)];k.CloseHandle.argtypes=[wintypes.HANDLE]
    snapshot=k.CreateToolhelp32Snapshot(2,0)
    if snapshot==ctypes.c_void_p(-1).value:return {root_pid}
    parents={};entry=Entry();entry.size=ctypes.sizeof(entry)
    try:
        more=k.Process32FirstW(snapshot,ctypes.byref(entry))
        while more:
            parents[entry.pid]=entry.parent
            more=k.Process32NextW(snapshot,ctypes.byref(entry))
    finally:k.CloseHandle(snapshot)
    found={root_pid}
    while True:
        next_set=found|{pid for pid,parent in parents.items() if parent in found}
        if next_set==found:return found
        found=next_set


def execute(command, directory, env=None, max_rss_bytes=12 * 1024**3, timeout=1800, json_output=True, family=False):
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=False)
    started, peak, stopped, family_peak, child_peak = time.perf_counter(), None, None, 0, 0
    with (directory / "stdout.json").open("w", encoding="utf-8") as out, (directory / "stderr.log").open("w", encoding="utf-8") as err:
        child = subprocess.Popen(command, cwd=ROOT, stdout=out, stderr=err, env=env)
        while child.poll() is None:
            measured = process_memory(child.pid)
            child_ids = descendants(child.pid) if family else {child.pid}
            family_current = sum(process_memory(pid, peak=False) or 0 for pid in child_ids)
            family_peak = max(family_peak, family_current)
            child_peak = max(child_peak, sum(process_memory(pid, peak=False) or 0 for pid in child_ids if pid != child.pid))
            if measured is not None:
                peak = max(peak or 0, measured)
                if (family_current if family else measured) > max_rss_bytes:
                    stopped = "memory ceiling exceeded"
            if time.perf_counter() - started > timeout:
                stopped = "wall time ceiling exceeded"
            if stopped:
                for pid in child_ids - {child.pid}:
                    try:os.kill(pid,9)
                    except (OSError,ProcessLookupError):pass
                child.kill()
                child.wait()
                break
            time.sleep(.05)
    result = {"command": command, "exit_code": child.returncode, "process_wall_seconds": time.perf_counter() - started,
              "peak_engine_rss_bytes": peak, "rss_sample_interval_ms": 50, "webview_memory": "not measured: headless engine probe",
              "stopped_reason": stopped, "memory_ceiling_bytes": max_rss_bytes, "timeout_seconds": timeout}
    if Path(command[0]).is_file():
        result['probe_sha256']=sha(command[0])
    if family:
        result['peak_family_working_set_bytes']=family_peak
        result['peak_webview_children_working_set_bytes']=child_peak
        result['webview_memory']='sampled descendant process working sets; 50 ms interval'
    if child.returncode == 0 and json_output:
        result["probe"] = json.loads((directory / "stdout.json").read_text(encoding="utf-8"))
    write_json(directory / "result.json", result)
    if child.returncode and not stopped:
        raise RuntimeError(f"probe failed; see {directory / 'stderr.log'}")
    return result


def run(args):
    manifest = json.loads(Path(args.manifest).read_text(encoding="utf-8"))
    csv = Path(args.csv).resolve()
    if sha(csv) != manifest["sha256"]:
        raise ValueError("input hash differs from fixture manifest")
    probe = Path(args.probe).resolve()
    if not probe.is_file():
        raise ValueError("probe executable does not exist; build it first")
    BUILD.mkdir(parents=True, exist_ok=True)
    run_dir = Path(tempfile.mkdtemp(prefix="run-", dir=BUILD))
    write_json(run_dir / "owner.json", {"purpose": "signal-atlas-benchmark", "schema": 1})
    environment = {"platform": platform.platform(), "cpu": platform.processor(), "logical_cpus": os.cpu_count(),
                   "go": subprocess.check_output(["go", "version"], text=True).strip(),
                   "python": sys.version, "probe_sha256": sha(probe), "fixture": manifest,
                   "filesystem_cache": "uncontrolled; app-cold and warm only", "threads": args.threads}
    if os.name == "nt":
        command = "Get-CimInstance Win32_ComputerSystem | Select-Object TotalPhysicalMemory,Model | ConvertTo-Json -Compress"
        environment["machine"] = json.loads(subprocess.check_output(["powershell.exe", "-NoProfile", "-Command", command], text=True))
    source_manifest = ROOT / "benchmarks" / "baselines" / "sources.json"
    if source_manifest.exists():
        environment["sources"] = json.loads(source_manifest.read_text())
    results = []
    if manifest['rows'] >= 50000000 and args.requests > 5:
        print('Capacity profile: five requests per trace; representative 10M traces use 200.', flush=True)
        args.requests = 5
    for trial in range(args.trials):
        trial_dir = run_dir / f"trial-{trial + 1}"
        store = trial_dir / "store"
        # Repeated imports need independent timing trials, not repeated copies
        # of the identical 200-request trace. Run full traces once per fixture.
        trace_requests = args.requests if trial == 0 else 1
        command = [str(probe), "-csv", str(csv), "-store", str(store), "-threads", str(args.threads), "-requests", str(trace_requests), "-fits", str(args.fits)]
        command += ["-expected-rows", str(manifest["baseline_expected_rows"]), "-bbox", args.bbox]
        if args.export:
            command.append("-export")
        if args.archive:
            command.append("-archive")
        print(f"Trial {trial + 1}/{args.trials}: {manifest['rows']:,} rows", flush=True)
        result = execute(command, trial_dir, max_rss_bytes=args.max_rss_gib * 1024**3)
        if not result["stopped_reason"]:
            result["cold_reopen"] = execute([str(probe), "-store", str(store), "-reopen-existing", "-requests", "1", "-threads", str(args.threads), "-bbox", args.bbox], trial_dir / "cold-reopen", max_rss_bytes=args.max_rss_gib * 1024**3)
        results.append(result)
        write_json(run_dir / "baseline.json", {"schema": 1, "environment": environment, "trials": results})
    print(run_dir / "baseline.json")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest="mode", required=True)
    f = sub.add_parser("freeze")
    f.add_argument("--donor", required=True)
    g = sub.add_parser("generate")
    g.add_argument("--csv", required=True)
    g.add_argument("--rows", type=int, default=1000000)
    g.add_argument("--radios", type=int, default=1000)
    g.add_argument("--high-cardinality", action="store_true")
    r = sub.add_parser("run")
    for name in ("csv", "manifest", "probe"):
        r.add_argument("--" + name, required=True)
    r.add_argument("--trials", type=int, default=3)
    r.add_argument("--threads", type=int, default=min(8, os.cpu_count() or 1))
    r.add_argument("--requests", type=int, default=200)
    r.add_argument("--fits", type=int, default=8)
    r.add_argument("--max-rss-gib", type=int, default=12)
    r.add_argument("--bbox", default="-123.401,48.399,-123.390,48.410")
    r.add_argument("--export", action="store_true")
    r.add_argument("--archive", action="store_true")
    n = sub.add_parser('native')
    n.add_argument('--data',required=True)
    n.add_argument('--page',choices=['gpu','explorer','donor'],required=True)
    n.add_argument('--probe',required=True)
    n.add_argument('--donor',required=True)
    n.add_argument('--seed-fits',type=int,default=0)
    args = p.parse_args()
    if args.mode == "freeze":
        BUILD.mkdir(parents=True, exist_ok=True)
        destination = Path(tempfile.mkdtemp(prefix="sources-", dir=BUILD))
        upstream = freeze(ROOT, destination / "upstream")
        donor = freeze(args.donor, destination / "donor", donor=True)
        # Publish hashes, not private absolute paths or data.
        write_json(ROOT / "benchmarks" / "baselines" / "sources.json", {"upstream": upstream, "donor": donor})
        print(destination)
    elif args.mode == "generate":
        if args.rows < 1 or args.radios < 1:
            p.error("rows and radios must be positive")
        print(json.dumps(generate(args.csv, args.rows, args.radios, not args.high_cardinality), indent=2))
    elif args.mode == 'native':
        BUILD.mkdir(parents=True,exist_ok=True)
        run_dir=Path(tempfile.mkdtemp(prefix='native-'+args.page+'-',dir=BUILD))
        output=run_dir/'native.json'
        command=[str(Path(args.probe).resolve()),'-data',str(Path(args.data).resolve()),'-repo',str(ROOT),'-donor',str(Path(args.donor).resolve()),'-page',args.page,'-output',str(output),'-seed-fits',str(args.seed_fits)]
        result=execute(command,run_dir/'process',max_rss_bytes=6*1024**3,timeout=150,json_output=False,family=True)
        result['probe_sha256']=sha(args.probe)
        result['instrumentation_sha256']=sha(ROOT/'benchmarks'/'native.js')
        if output.exists():result['native']=json.loads(output.read_text())
        else:result['native']={'errors':['no performance report produced']}
        write_json(run_dir/'result.json',result)
        print(run_dir/'result.json')
    else:
        if args.trials < 1 or args.requests < 1 or args.threads < 1 or args.fits < 0:
            p.error("invalid trial, request, worker or fit count")
        run(args)


if __name__ == "__main__":
    main()
