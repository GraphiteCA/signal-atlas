"""Serve the large-survey explorer locally. No observations are uploaded externally."""
from __future__ import annotations

import argparse
from collections import Counter
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import sqlite3
import tempfile
import threading
import urllib.parse
import uuid

import numpy as np

from area_estimates import area_estimates, area_plan
from estimate_ap import LocationModel
from selection_estimates import (annotate_bands, comparisons, delete_comparison, estimation_groups,
                                save_comparison, selected_groups, unknown_band_aliases)
from survey_data import (assign_band, available_dates, backup_database, connect, database_info, estimate_scope, filters, import_csv, initialize, load_basemap,
                         networks, observations, points, radios, summary, transmitters)

ROOT = Path(__file__).resolve().parent


class Jobs:
    """One background import/fit job at a time, with bounded progress messages."""
    def __init__(self, database):
        self.database = database
        self.lock = threading.Lock()
        self.state = {"id": None, "status": "idle", "kind": None}
        self.cancel = threading.Event()

    def snapshot(self):
        with self.lock:
            return dict(self.state)

    def update(self, **fields):
        with self.lock:
            self.state.update(fields)

    def start(self, kind, worker):
        with self.lock:
            if self.state["status"] == "running":
                raise ValueError("Another background job is running; wait or cancel it first")
            self.cancel.clear()
            self.state = {"id": uuid.uuid4().hex, "status": "running", "kind": kind, "completed": 0, "total": 0}
        def run():
            try:
                result = worker()
                self.update(status="cancelled" if self.cancel.is_set() else "complete", result=result)
            except Exception as exc:
                self.update(status="cancelled" if self.cancel.is_set() else "failed", error=str(exc))
        threading.Thread(target=run, daemon=True).start()
        return self.snapshot()

    def import_file(self, path, filename, skip_invalid):
        def worker():
            try:
                def progress(valid, invalid, read, total):
                    if self.cancel.is_set():
                        raise ValueError("Import cancelled; changes rolled back")
                    self.update(completed=valid, invalid=invalid, bytes_read=read, bytes_total=total)
                return import_csv(path, self.database, skip_invalid, progress, filename)
            finally:
                path.unlink(missing_ok=True)
        return self.start("import", worker)

    def estimate(self, params, groups=None, kind='estimate'):
        scope, scope_key = estimate_scope(params)
        # Validate a shared fit before starting a background worker.
        if groups is None:
            selected = selected_groups(self.database, params)
            groups = estimation_groups(selected, params)
        eligible = len(groups)
        if params.get('missing') == ['1']:
            with connect(self.database) as db:
                cached = {r['radio_key'] for r in db.execute('SELECT radio_key FROM estimates WHERE scope_key=?', (scope_key,))}
                groups = [g for g in groups if g['radio_key'] not in cached]
        def worker():
            self.update(total=len(groups), observation_scope=scope)
            fitted = skipped = 0
            for index, group in enumerate(groups):
                if self.cancel.is_set():
                    break
                result = self.fit_radio(group, params)
                with connect(self.database) as db:
                    db.execute("INSERT OR REPLACE INTO estimates(radio_key,scope_key,result) VALUES(?,?,?)", (group["radio_key"], scope_key, json.dumps(result, allow_nan=False)))
                if result["status"] == "fitted":
                    fitted += 1
                else:
                    skipped += 1
                self.update(completed=index+1, fitted=fitted, skipped=skipped)
            return {"fitted": fitted, "skipped": skipped, "eligible_radios": eligible, "attempted_fits": len(groups)}
        return self.start(kind, worker)

    def fit_radio(self, group, params=None):
        params = params or {}
        scope, _ = estimate_scope(params)
        _, where, values = filters(params)
        members = group.get('members', [group])
        if not group.get('ssid') and len({r['mac'].upper() for r in members}) > 1:
            raise ValueError('Hidden SSIDs must be fitted separately by BSSID/MAC address')
        keys = [r['radio_key'] for r in members]
        where = f"o.radio_key IN ({','.join('?' for _ in keys)}) AND ({where})"
        values = [*keys, *values]
        shared = group.get("co_location_assumed", "members" in group)
        result = {**group, "status": "insufficient", "co_location_assumed": shared,
                  "sample_limit": 256*len(keys) if scope["fit_mode"]=="sample" else None,
                  "fit_mode": scope["fit_mode"], "observation_scope": scope}
        with connect(self.database) as db:
            db.set_progress_handler(lambda: int(self.cancel.is_set()), 10000)
            counts = dict(db.execute(f"SELECT count(*) count,min(date) first_date,max(date) last_date FROM observations o WHERE {where}", values).fetchone())
            result.update(count=counts["count"], first_date=counts["first_date"], last_date=counts["last_date"])
            if counts["count"] < 4:
                result["reason"] = "Fewer than four observations within the selected dates and filters"
                return result
            if scope["fit_mode"] == "all":
                sample = [dict(r) for r in db.execute(f'''SELECT min(id) id,radio_key,mac,latitude,longitude,avg(rssi_dbm) rssi_dbm,
                    count(*) weight,max(0,total(rssi_dbm*rssi_dbm)-total(rssi_dbm)*total(rssi_dbm)/count(*)) within_sse
                    FROM observations o WHERE {where} GROUP BY radio_key,latitude,longitude ORDER BY min(id)''', values)]
                weights = [r["weight"] for r in sample]
                within = [r["within_sse"] for r in sample]
            elif counts["count"] <= 256:
                sample = [dict(r) for r in db.execute(f"SELECT * FROM observations o WHERE {where} ORDER BY id", values)]
                weights = within = None
            else:
                sample = []
                for key in keys:
                    radio_where = f'o.radio_key=? AND ({where})'
                    radio_values = [key, *values]
                    strongest = db.execute(f"SELECT * FROM observations o WHERE {radio_where} ORDER BY rssi_dbm DESC,id LIMIT 32", radio_values).fetchall()
                    spread = db.execute(f"SELECT * FROM observations o WHERE {radio_where} ORDER BY ((id*1103515245+12345)%2147483647) LIMIT 224", radio_values).fetchall()
                    sample.extend(dict(r) for r in {r["id"]: r for r in [*strongest, *spread]}.values())
                weights = within = None
        sample = [dict(r, key=str(r["id"])) for r in sample]
        if shared:
            # Independent power intercept for every BSSID/band, including when
            # the same MAC was reported on more than one band.
            aliases = unknown_band_aliases(group['members'])
            sample = [dict(r, mac=aliases.get(r['radio_key'], r['radio_key'])) for r in sample]
        used = counts["count"] if scope["fit_mode"]=="all" else len(sample)
        result.update(sampled_observations=used, used_observations=used, coordinate_groups=len(sample))
        try:
            model = LocationModel(sample, weights=weights, within_sse=within, cancelled=self.cancel.is_set)
            fit = model.fit(np.arange(len(sample)), 3., extent_m=500, step_m=10, include_predictions=False)
            result.update(status="fitted", latitude=fit["latitude"], longitude=fit["longitude"],
                          residual_rms_db=fit["residual_rms_db"], boundary_limited=fit["boundary_limited"],
                          path_loss_exponent=3.,
                          note=("All matching observations; identical GPS positions are exactly aggregated. " if scope["fit_mode"]=="all" else "Fast sampled estimate. ")+"Fixed-power, stationary-radio model. Planning radius is not a confidence interval.")
        except ValueError as exc:
            result["reason"] = str(exc)
        return result


def fitted_estimates(database, params):
    scope, scope_key = estimate_scope(params)
    limit = max(1, min(500, int(params.get("limit", ["200"])[0])))
    groups = selected_groups(database, params)
    targets = estimation_groups(groups, params)
    with connect(database) as db:
        # Date windows, selected days, signal thresholds and sampling modes get
        # separate cache entries. A full-history fit never stands in for a dated fit.
        # Check just the requested keys and load JSON only for the visible page.
        target_keys = sorted(g['radio_key'] for g in targets)
        results = []
        found = 0
        for start in range(0, len(target_keys), 400):
            batch = target_keys[start:start + 400]
            slots = ','.join('?' for _ in batch)
            keys = [row[0] for row in db.execute(
                f'SELECT radio_key FROM estimates WHERE scope_key=? AND radio_key IN ({slots}) ORDER BY radio_key',
                [scope_key, *batch])]
            found += len(keys)
            page = keys[:limit - len(results)]
            if page:
                page_slots = ','.join('?' for _ in page)
                results.extend(json.loads(row[0]) for row in db.execute(
                    f'SELECT result FROM estimates WHERE scope_key=? AND radio_key IN ({page_slots}) ORDER BY radio_key',
                    [scope_key, *page]))
        missing = len(target_keys) - found
        annotate_bands(db, results)
    shared_limited = params.get('shared') == ['1'] and any(n>20 for n in Counter(g['ssid'] for g in groups).values())
    return {"estimates": results, "more": found>limit, "observation_scope": scope,
            "shared_warning": "Shared fits skipped for SSIDs with more than 20 selected radios; narrow those selections." if shared_limited else "",
            "eligible_radios": sum(g['count']>=4 for g in groups), "eligible_fits": len(targets),
            "selected_radios": len(groups), "missing_estimates": missing}


class SurveyServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, database):
        self.database = Path(database).resolve()
        initialize(self.database)
        self.jobs = Jobs(self.database)
        super().__init__(address, Handler)


class Handler(BaseHTTPRequestHandler):
    server_version = "SignalAtlas/2.0"

    def log_message(self, format, *args):
        pass

    def handle(self):
        try:
            super().handle()
        except (ConnectionError, TimeoutError):
            # Map interactions intentionally abort stale requests. A disconnected
            # client is not an application error and must not trigger a second reply.
            pass

    def send_json(self, body, status=200):
        payload = json.dumps(body, allow_nan=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def valid_host(self):
        # The viewer is loopback-only; do not accept cross-site/DNS-rebinding
        # access to local survey data. No permissive CORS headers are emitted.
        allowed = {f"127.0.0.1:{self.server.server_port}", f"localhost:{self.server.server_port}"}
        return self.headers.get("Host") in allowed

    def do_GET(self):
        if not self.valid_host():
            return self.send_json({"error": "Use the local viewer address"}, 403)
        url = urllib.parse.urlsplit(self.path)
        params = urllib.parse.parse_qs(url.query, keep_blank_values=True)
        try:
            routes = {"/api/summary": summary, "/api/networks": networks, "/api/transmitters": transmitters, "/api/radios": radios,
                      "/api/dates": available_dates, "/api/area-estimates": area_estimates,
                      "/api/points": points, "/api/observations": observations, "/api/estimates": fitted_estimates}
            if url.path in routes:
                return self.send_json(routes[url.path](self.server.database, params))
            if url.path == "/api/database":
                return self.send_json(database_info(self.server.database))
            if url.path == "/api/database/download":
                # A backup snapshot is safe while WAL-mode imports/estimates are
                # active. Copying the live .sqlite3 file alone could omit WAL data.
                with tempfile.TemporaryDirectory(dir=self.server.database.parent, prefix="snapshot-") as directory:
                    path = backup_database(self.server.database, Path(directory)/"survey.sqlite3")
                    self.send_response(200)
                    self.send_header("Content-Type", "application/vnd.sqlite3")
                    self.send_header("Content-Disposition", 'attachment; filename="signal-atlas.sqlite3"')
                    self.send_header("Cache-Control", "no-store")
                    self.send_header("Content-Length", str(path.stat().st_size))
                    self.end_headers()
                    with path.open("rb") as source:
                        for block in iter(lambda: source.read(1024*1024), b""):
                            self.wfile.write(block)
                return
            if url.path == "/api/job":
                return self.send_json(self.server.jobs.snapshot())
            if url.path == "/api/basemap":
                return self.send_json(load_basemap(self.server.database))
            if url.path == '/api/comparisons':
                return self.send_json(comparisons(self.server.database))
            assets = {"/": ("explorer.html", "text/html"), "/explorer.js": ("explorer.js", "text/javascript"),
                      "/explorer.css": ("explorer.css", "text/css"), "/satellite.js": ("satellite.js", "text/javascript"),
                      "/network_tree.js": ("network_tree.js", "text/javascript"),
                      "/observation_scope.js": ("observation_scope.js", "text/javascript"),
                      "/estimate_comparison.js": ("estimate_comparison.js", "text/javascript"),
                      "/online_map.js": ("online_map.js", "text/javascript"),
                      "/follow_estimate.js": ("follow_estimate.js", "text/javascript"),
                      "/area_estimates.js": ("area_estimates.js", "text/javascript"),
                      "/network_tree.css": ("network_tree.css", "text/css")}
            if url.path not in assets:
                return self.send_json({"error": "Not found"}, 404)
            name, mime = assets[url.path]
            content = (ROOT/"templates"/name).read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", mime+"; charset=utf-8")
            self.send_header("Content-Length", str(len(content)))
            self.send_header("Referrer-Policy", "strict-origin-when-cross-origin")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.end_headers()
            self.wfile.write(content)
        except (ConnectionError, TimeoutError):
            return
        except (ValueError, sqlite3.Error, OSError) as exc:
            self.send_json({"error": str(exc)}, 400)

    def do_POST(self):
        if not self.valid_host():
            return self.send_json({"error": "Use the local viewer address"}, 403)
        origin = self.headers.get("Origin")
        if origin and origin not in {f"http://127.0.0.1:{self.server.server_port}", f"http://localhost:{self.server.server_port}"}:
            return self.send_json({"error": "Use the local viewer page"}, 403)
        url = urllib.parse.urlsplit(self.path)
        try:
            if url.path in ('/api/comparisons', '/api/comparisons/delete'):
                size = int(self.headers.get('Content-Length', '0'))
                if not 0 < size <= 65536:
                    raise ValueError('Invalid comparison request size')
                body = json.loads(self.rfile.read(size))
                if not isinstance(body, dict):
                    raise ValueError('Expected a comparison object')
                if url.path.endswith('/delete'):
                    delete_comparison(self.server.database, str(body.get('id', '')))
                else:
                    params = urllib.parse.parse_qs(url.query, keep_blank_values=True)
                    save_comparison(self.server.database, params, body.get('name'), fitted_estimates)
                return self.send_json(comparisons(self.server.database))
            if url.path == "/api/cancel":
                self.server.jobs.cancel.set()
                return self.send_json(self.server.jobs.snapshot())
            if self.server.jobs.snapshot()["status"] == "running":
                return self.send_json({"error": "A background job is already running"}, 409)
            if url.path == '/api/band-assignment':
                size=int(self.headers.get('Content-Length','0'))
                if not 0 < size <= 4096:
                    raise ValueError('Invalid band assignment size')
                body=json.loads(self.rfile.read(size))
                if not isinstance(body,dict):
                    raise ValueError('Expected a band assignment object')
                return self.send_json(assign_band(self.server.database,body.get('ssid'),body.get('mac'),body.get('band')))
            if url.path == "/api/import":
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 2_000_000_000:
                    raise ValueError("Choose a CSV between 1 byte and 2 GB")
                filename = Path(urllib.parse.unquote(self.headers.get("X-Filename", "survey.csv"))).name
                self.connection.settimeout(120)
                with tempfile.NamedTemporaryFile(dir=self.server.database.parent, prefix="upload-", suffix=".csv", delete=False) as temporary:
                    path = Path(temporary.name)
                    try:
                        remaining = size
                        while remaining:
                            block = self.rfile.read(min(1024*1024, remaining))
                            if not block:
                                raise ValueError("Upload ended before the whole CSV arrived")
                            temporary.write(block)
                            remaining -= len(block)
                    except Exception:
                        temporary.close()
                        path.unlink(missing_ok=True)
                        raise
                try:
                    job = self.server.jobs.import_file(path, filename, self.headers.get("X-Skip-Invalid") == "1")
                except Exception:
                    path.unlink(missing_ok=True)
                    raise
                return self.send_json(job, 202)
            if url.path == '/api/area-estimate':
                params = urllib.parse.parse_qs(url.query, keep_blank_values=True)
                plan, scoped, groups = area_plan(self.server.database, params, display=False)
                if not groups:
                    return self.send_json({**self.server.jobs.snapshot(), 'area_more': plan['more']})
                job = self.server.jobs.estimate(scoped, groups=groups, kind='area-estimate')
                return self.send_json({**job, 'area_more': plan['more'], 'area_started': True}, 202)
            if url.path == "/api/estimate":
                params = urllib.parse.parse_qs(url.query, keep_blank_values=True)
                filters(params)  # Validate before queuing a long operation.
                return self.send_json(self.server.jobs.estimate(params), 202)
            self.send_json({"error": "Not found"}, 404)
        except (ConnectionError, TimeoutError):
            return
        except (ValueError, OSError, sqlite3.Error) as exc:
            self.send_json({"error": str(exc)}, 400)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", type=Path, default=Path("outputs/survey.sqlite3"))
    parser.add_argument("--port", type=int, default=8765)
    args = parser.parse_args()
    with SurveyServer(("127.0.0.1", args.port), args.database) as server:
        print(f"Signal Atlas: http://127.0.0.1:{server.server_port}/", flush=True)
        print(f"Database: {server.database}\nCtrl+C to stop. Survey data stays local.", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
