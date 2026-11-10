"""SQLite-first entry point for Signal Atlas: initialize, import, locate, serve, back up."""
from __future__ import annotations
import argparse
import json
import sqlite3
from pathlib import Path
import time
from survey_data import backup_database, database_info, discover_csv_files, import_csv, initialize, store_basemap


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", type=Path, default=Path("outputs/survey.sqlite3"))
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("init", help="Initialize or migrate the canonical database")
    commands.add_parser("status", help="Describe the database; no CSV or JSON report required")
    importer = commands.add_parser("import", help="Append CSV/WiGLE files or a folder to SQLite")
    importer.add_argument("sources", nargs="+", type=Path)
    importer.add_argument("--skip-invalid", action="store_true")
    viewer = commands.add_parser("serve", help="Open the SQLite-backed local viewer")
    viewer.add_argument("--port", type=int, default=8765)
    locate = commands.add_parser("locate", help="Fit radios directly from SQLite, optionally targeting MACs")
    locate.add_argument("--ssid", required=True)
    locate.add_argument("--mac", action="append", help="BSSID/MAC to locate; repeat to select multiple MACs")
    locate.add_argument("--band", action="append", choices=("2.4", "5", "6", "unknown", "other"))
    locate.add_argument("--date", action="append", help="Use only this observation date (YYYY-MM-DD); repeat for multiple days")
    locate.add_argument("--date-from", help="Inclusive first observation date, YYYY-MM-DD")
    locate.add_argument("--date-to", help="Inclusive last observation date, YYYY-MM-DD")
    locate.add_argument("--shared", action="store_true", help="Also fit one shared location, assuming the selected radios are colocated")
    locate.add_argument("--fit-mode", choices=("all", "sample"), default="all", help="Use every matching observation, or an explicit fast sample")
    locate.add_argument("--output", type=Path, help="Optional JSON export; estimates always remain in SQLite")
    backup = commands.add_parser("backup", help="Create a consistent standalone SQLite snapshot")
    backup.add_argument("output", type=Path)
    basemap = commands.add_parser("basemap", help="Store a local GeoJSON map excerpt inside SQLite")
    basemap.add_argument("geojson", type=Path)
    args = parser.parse_args()
    try:
        if args.command == "init":
            initialize(args.database)
            print(json.dumps(database_info(args.database), indent=2))
        elif args.command == "backup":
            print(backup_database(args.database, args.output).resolve())
        elif args.command == "basemap":
            store_basemap(args.database, args.geojson)
            print("Stored local map context in SQLite")
        elif args.command == "status":
            if not args.database.exists():
                raise ValueError("Database not found; run survey.py init first")
            initialize(args.database)
            print(json.dumps(database_info(args.database), indent=2))
        elif args.command == "import":
            paths = discover_csv_files(args.sources)
            if not paths:
                raise ValueError("No CSV surveys found")
            for path in paths:
                print(f"Importing {path.name}", flush=True)
                print(json.dumps(import_csv(path, args.database, args.skip_invalid)))
        elif args.command == "serve":
            from serve_survey import SurveyServer
            with SurveyServer(("127.0.0.1", args.port), args.database) as server:
                print(f"Signal Atlas: http://127.0.0.1:{server.server_port}/\nSQLite: {server.database}", flush=True)
                try:
                    server.serve_forever()
                except KeyboardInterrupt:
                    pass
        elif args.command == "locate":
            from serve_survey import Jobs, fitted_estimates
            initialize(args.database)
            params = {"ssid": [args.ssid]}
            if args.shared:
                params["shared"] = ["1"]
            if args.mac:
                params["mac"] = args.mac
            if args.band:
                params["band"] = args.band
            params["fit_mode"] = [args.fit_mode]
            for name in ("date", "date_from", "date_to"):
                value = getattr(args, name)
                if value:
                    params[name] = value if isinstance(value, list) else [value]
            jobs = Jobs(args.database)
            jobs.estimate(params)
            try:
                while jobs.snapshot()["status"] == "running":
                    time.sleep(.1)
            except KeyboardInterrupt:
                jobs.cancel.set()
                while jobs.snapshot()["status"] == "running":
                    time.sleep(.1)
                raise ValueError("Location job cancelled; completed fits remain in SQLite")
            job = jobs.snapshot()
            if job["status"] == "failed":
                raise ValueError(job["error"])
            report = fitted_estimates(args.database, {**params, "limit": ["500"]})
            report["job"] = job["result"]
            if args.output:
                args.output.parent.mkdir(parents=True, exist_ok=True)
                args.output.write_text(json.dumps(report, indent=2)+"\n", encoding="utf-8")
            print(json.dumps(report, indent=2))
    except (OSError, ValueError, sqlite3.Error) as exc:
        parser.exit(2, f"error: {exc}\n")


if __name__ == "__main__":
    main()
