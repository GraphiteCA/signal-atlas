"""Stream a large multi-SSID CSV into Signal Atlas's local SQLite database."""
import argparse
from pathlib import Path
from survey_data import discover_csv_files, import_csv


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("csv", type=Path, nargs="+", help="One or more CSV files, or folders containing CSVs")
    parser.add_argument("--database", type=Path, default=Path("outputs/survey.sqlite3"))
    parser.add_argument("--skip-invalid", action="store_true", help="Skip and report invalid rows; default is atomic rejection")
    args = parser.parse_args()
    try:
        paths = discover_csv_files(args.csv)
        if not paths:
            raise ValueError("No CSV files found")
        for path in paths:
            print(f"Importing {path.name}", flush=True)
            result = import_csv(path, args.database, args.skip_invalid,
                progress=lambda n, bad, read, total: print(f"{n:,} observations | {bad:,} invalid | {read/max(total,1):.0%}", flush=True))
            print(result)
    except (OSError, ValueError) as exc:
        parser.exit(2, f"error: {exc}\n")


if __name__ == "__main__":
    main()
