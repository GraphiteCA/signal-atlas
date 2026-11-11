"""Full-survey metadata comparison on independent owned benchmark copies.

Usage: python benchmarks/metadata_perf.py --source-owned <unfitted-owned-store>
The source must already have a benchmark ownership marker; production surveys
are never opened or reset by this runner. No existing destination is overwritten.
"""
import argparse
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

import phase2
import run


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-owned', type=Path, required=True)
    parser.add_argument('--base', default='e60214a')
    args = parser.parse_args()
    source = args.source_owned.resolve(strict=True)
    # Validate ownership with the same guard as every independent clone.
    root = Path(tempfile.mkdtemp(prefix='metadata-', dir=run.BUILD))
    run.write_json(root/'owner.json', {'purpose': 'signal-atlas-benchmark', 'schema': 1})
    phase2.clone_workspace(source, root/'before-data/store')
    phase2.clone_workspace(source, root/'after-data/store')
    archive = subprocess.check_output(['git', 'archive', args.base], cwd=run.ROOT)
    with tarfile.open(fileobj=io.BytesIO(archive)) as tf:
        tf.extractall(root/'base-source', filter='data')
    manifest = run.freeze(run.ROOT, root/'current-source')
    report = {'schema': 1, 'base': args.base, 'current_source': manifest['source_sha256'], 'runs': {}}
    env = {**os.environ, 'GOTOOLCHAIN': 'go1.27.3'}
    for version, tree in [('before', 'base-source'), ('after', 'current-source')]:
        binary = root/f'prep-{version}.exe'
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/fullprepbench'],
                       cwd=root/tree, env=env, check=True)
        result = run.execute([str(binary), '-data', str(root/f'{version}-data'),
                              '-solver', 'fast', '-gpu', '-workers', '8',
                              '-profile', str(root/f'{version}.cpu')], root/f'{version}-full',
                             max_rss_bytes=6 << 30, timeout=1800)
        if result['exit_code'] or result['stopped_reason']:
            raise RuntimeError(result)
        probe = result['probe']
        if probe['metrics']['progress']['pending'] or probe['acceleration']['fallbacks']:
            raise RuntimeError('Incomplete preparation or GPU fallback')
        report['runs'][version] = result
        run.write_json(root/'summary.json', report)
    before, after = [report['runs'][v]['probe'] for v in ('before', 'after')]
    for key in ('rows', 'stats', 'saved', 'estimates_sha256'):
        if before[key] != after[key]:
            raise RuntimeError(f'Durable result mismatch: {key}')
    print(root/'summary.json')


if __name__ == '__main__':
    main()
