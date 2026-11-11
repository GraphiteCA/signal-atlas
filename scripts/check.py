"""Local PR checks; release creation/upload remains a separate maintainer action.

Run with a Python that has NumPy installed. --race needs a working C toolchain;
--gpu requires a supported physical OpenCL device; --cross compiles CPU fallbacks.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]


def run(args, cwd=ROOT, env=None):
    print('+', ' '.join(map(str, args)), flush=True)
    subprocess.run(list(map(str, args)), cwd=cwd, env=env, check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--race', action='store_true')
    parser.add_argument('--gpu', action='store_true')
    parser.add_argument('--cross', action='store_true')
    args = parser.parse_args()
    for tool in ('go', 'node'):
        if not shutil.which(tool):
            raise SystemExit(f'{tool} is required; parity checks must not silently skip')
    run([sys.executable, '-c', 'import numpy; print("NumPy", numpy.__version__)'])
    env = {**os.environ, 'PATH': str(Path(sys.executable).parent)+os.pathsep+os.environ.get('PATH', '')}
    for key in ('SIGNAL_ATLAS_QUALITY_REPORT', 'SIGNAL_ATLAS_PHASE5_HARDWARE_REPORT'):
        env.pop(key, None)
    manifest = json.loads((ROOT/'ui/vendor/manifest.json').read_text(encoding='utf-8'))
    for name, expected in manifest['sha256'].items():
        actual = hashlib.sha256((ROOT/'ui/vendor'/name).read_bytes()).hexdigest()
        if actual != expected:
            raise SystemExit(f'Bundled asset hash mismatch: {name}')
    print('Bundled map assets match their pinned manifest', flush=True)
    run(['go', 'test', '-count=1', './...'], env=env)
    run(['go', 'vet', './...'], env=env)
    run([sys.executable, '-m', 'unittest', 'discover', '-s', 'tests'], cwd=ROOT/'python', env=env)
    run(['node', '--test', 'benchmarks/map-display.test.cjs'], env=env)
    run([sys.executable, '-m', 'unittest', 'discover', '-s', 'benchmarks', '-p', 'test_*.py'], env=env)
    if args.race:
        run(['go', 'test', '-race', './internal/obs', './internal/atlas', './internal/engine', './internal/gpu',
             '-run', 'Preparation|Viewport|PanReprioritizes|Map|Location|Hidden|Queued|Grid|Dictionary', '-count=1'],
            env={**env, 'CGO_ENABLED': '1'})
    if args.gpu:
        run(['go', 'test', '-tags', 'gpuintegration', './internal/atlas', '-run',
             '^TestOpenCL(HardwareScoringAndSolvers|KnownTruthMatrix)$', '-count=1'], env=env)
    if args.cross:
        for target, arch in [('linux', 'amd64'), ('darwin', 'arm64')]:
            run(['go', 'build', './internal/engine', './internal/gpu'],
                env={**env, 'CGO_ENABLED': '0', 'GOOS': target, 'GOARCH': arch})
    print('Requested checks passed', flush=True)


if __name__ == '__main__':
    main()
