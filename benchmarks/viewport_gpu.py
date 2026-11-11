"""Paired real OpenCL and viewport-first preparation probes on owned fixtures.

Build cmd/gpubench and cmd/viewportbench against the base and current revisions
as gpu-before.exe/gpu-final.exe and viewport-before.exe/viewport-after.exe in
an owned benchmark root. The baseline gpubench has no CandidateGrid API.
"""
import argparse
from pathlib import Path

import phase2
import run


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve(strict=True)
    import json
    if json.loads((root/'owner.json').read_text()) != {'purpose': 'signal-atlas-benchmark', 'schema': 1}:
        raise ValueError('Owned benchmark root required')
    report = {'schema': 1, 'gpu': [], 'viewport': []}

    def execute(name, command):
        result = run.execute(list(map(str, command)), root/name, timeout=180, max_rss_bytes=6 << 30)
        if result['exit_code'] or result['stopped_reason']:
            raise RuntimeError(result)
        return result

    for rows in (8, 50, 256, 2048):
        for trial in range(3):
            paired = {}
            for version in (('before', 'final') if trial % 2 == 0 else ('final', 'before')):
                result = execute(f'fixed-{version}-{rows}-{trial}', [root/f'gpu-{version}.exe', '-rows', rows, '-loops', 60 if rows == 2048 else 300])
                paired[version] = result
            if paired['before']['probe']['score_sha256'] != paired['final']['probe']['score_sha256']:
                raise RuntimeError('GPU score bit patterns changed')
            report['gpu'].append({'rows': rows, 'trial': trial, **paired})
            run.write_json(root/'final-summary.json', report)

    source = root/'viewport-source-data'
    execute('viewport-fixture', [root/'viewport-after.exe', '-data', source, '-build'])
    for trial in range(3):
        paired = {}
        for version in (('before', 'after') if trial % 2 == 0 else ('after', 'before')):
            data = root/f'viewport-{version}-data-{trial}'
            phase2.clone_workspace(source/'store', data/'store')
            paired[version] = execute(f'viewport-{version}-{trial}', [root/f'viewport-{version}.exe', '-data', data])
            if paired[version]['probe']['local_map_aps'] != 64:
                raise RuntimeError('Local locations missing')
        report['viewport'].append({'trial': trial, **paired})
        run.write_json(root/'final-summary.json', report)
    print(root/'final-summary.json')


if __name__ == '__main__':
    main()
