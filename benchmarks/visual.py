"""Run a browser smoke test against an owned benchmark survey only."""
import argparse
import json
import socket
import subprocess
import tempfile
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def ready(url):
    for _ in range(150):
        try:
            urllib.request.urlopen(url, timeout=1).read()
            return
        except (OSError, TimeoutError):
            time.sleep(.1)
    raise RuntimeError('Test server did not become ready: ' + url)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--data', required=True)
    parser.add_argument('--app', default='build/SignalAtlas-phase1.exe')
    parser.add_argument('--driver', choices=['map-smoke.cjs','admin-smoke.cjs'], default='map-smoke.cjs')
    parser.add_argument('--chrome', default=r'C:\Program Files\Google\Chrome\Application\chrome.exe')
    args = parser.parse_args()
    data = Path(args.data).resolve(strict=True)
    markers = [p / 'owner.json' for p in [data, *list(data.parents)[:3]]]
    def owned(marker):
        if not marker.is_file():
            return False
        value = json.loads(marker.read_text())
        return value.get('purpose') == 'signal-atlas-benchmark' and value.get('schema') == 1
    if not any(owned(p) for p in markers):
        raise ValueError('An owned benchmark fixture is required')
    folder = Path(tempfile.mkdtemp(prefix='phase1-ui-', dir=ROOT / 'build' / 'benchmarks'))
    app_port, debug_port = port(), port()
    chrome = None
    with (folder / 'server.log').open('w') as log, (folder / 'chrome.log').open('w') as chrome_log:
        server = subprocess.Popen([str(Path(args.app).resolve()), '-serve', f'127.0.0.1:{app_port}', '-data', str(data)], stdout=log, stderr=log)
        try:
            ready(f'http://127.0.0.1:{app_port}/api/app/info')
            chrome = subprocess.Popen([args.chrome, '--headless=new', '--no-first-run', '--no-default-browser-check',
                                       f'--user-data-dir={folder / "chrome-profile"}', f'--remote-debugging-port={debug_port}',
                                       '--window-size=1384,911', 'about:blank'], stdout=chrome_log, stderr=chrome_log)
            ready(f'http://127.0.0.1:{debug_port}/json/version')
            result = subprocess.run(['node', str(ROOT / 'benchmarks' / args.driver), str(debug_port),
                                     f'http://127.0.0.1:{app_port}', str(folder)], capture_output=True, timeout=120)
            (folder / 'smoke.json').write_bytes(result.stdout)
            (folder / 'smoke.log').write_bytes(result.stderr)
            print(folder)
            print(result.stdout.decode())
            if result.returncode:
                raise RuntimeError(result.stderr.decode())
        finally:
            if chrome and chrome.poll() is None:
                chrome.terminate()
                chrome.wait(timeout=10)
            server.terminate()
            server.wait(timeout=10)


if __name__ == '__main__':
    main()
