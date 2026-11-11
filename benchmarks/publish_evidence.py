"""Prepare benchmark JSON for publication without copying private source paths.

Raw reports stay in ignored build/. Measurements, failures and artifact hashes
are retained; long timing/progress series become explicit statistical summaries.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import statistics


class Publisher:
    def __init__(self, reports, checkout):
        self.checkout = str(Path(checkout).resolve()).replace('\\', '/')
        self.donor = str(Path(checkout).resolve().parent).replace('\\', '/')
        names = set()

        def discover(value):
            if isinstance(value, dict):
                for key, item in value.items():
                    discover(key)
                    discover(item)
            elif isinstance(value, list):
                for item in value:
                    discover(item)
            elif isinstance(value, str):
                for path in value.replace('\\', '/').split(';'):
                    if '/inputs/' in path.lower() and path.lower().endswith('.csv'):
                        names.add(path.rsplit('/', 1)[-1])

        for report in reports:
            discover(report)
        self.names = {name: f'private-input-{i:02d}.csv' for i, name in enumerate(sorted(names), 1)}

    def text(self, value):
        for name, replacement in self.names.items():
            value = value.replace(name, replacement)
        # Normalize whole path values, never arbitrary source/prose containing
        # a URL or a Windows example: that would corrupt regex/escape sequences.
        if re.match(r'^[A-Za-z]:[/\\]', value):
            value = value.replace('\\', '/')
        for prefix, replacement in [(self.checkout, '<checkout>'), (self.donor, '<donor-checkout>')]:
            pattern = re.escape(prefix).replace('/', r'[/\\]+') + r'(?=[/\\]|$)'
            value = re.sub(pattern, replacement, value, flags=re.I)
        value = re.sub(r'(?<![A-Za-z0-9])[A-Za-z]:[/\\]+Users[/\\]+[^/\\\s]+[/\\]+[^;\n]*', '<private-path>', value, flags=re.I)
        return value

    def report(self, value):
        if isinstance(value, str):
            return self.text(value)
        if isinstance(value, list):
            return [self.report(v) for v in value]
        if not isinstance(value, dict):
            return value
        out = {}
        for key, item in value.items():
            key = self.text(key)
            if key in out:
                raise ValueError('Publication would merge distinct keys')
            series = key in {'runs_ms', 'delivery_ms', 'samples'} and isinstance(item, list) and len(item) > 40
            numeric = series and all(isinstance(v, (int, float)) for v in item)
            progress = series and all(isinstance(v, dict) and set(v) <= {'seconds', 'completed'} for v in item)
            if numeric or progress:
                summary = {'count': len(item), 'raw_series_omitted': True,
                           'raw_series_sha256': hashlib.sha256(json.dumps(item, sort_keys=True, separators=(',', ':')).encode()).hexdigest(),
                           'first': item[0], 'last': item[-1]}
                if numeric:
                    ordered = sorted(item)
                    summary.update(minimum=ordered[0], median=statistics.median(ordered),
                                   p95=ordered[(len(ordered)*95+99)//100-1], maximum=ordered[-1])
                else:
                    summary['first_completed_seconds'] = next((v.get('seconds') for v in item if v.get('completed', 0) > 0), None)
                out[key+'_summary'] = summary
            else:
                out[key] = self.report(item)
        return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    files = sorted(args.source.glob('*.json'))
    reports = [json.loads(p.read_text(encoding='utf-8')) for p in files]
    publisher = Publisher(reports, Path(__file__).resolve().parents[1])
    args.output.mkdir(parents=True, exist_ok=False)
    for path, report in zip(files, reports):
        (args.output/path.name).write_text(json.dumps(publisher.report(report), indent=2, ensure_ascii=False)+'\n', encoding='utf-8')


if __name__ == '__main__':
    main()
