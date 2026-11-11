import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import sys

spec = importlib.util.spec_from_file_location("baseline", Path(__file__).with_name("run.py"))
baseline = importlib.util.module_from_spec(spec)
spec.loader.exec_module(baseline)


class BenchmarkSafety(unittest.TestCase):
    def test_fixture_reproducible_and_exclusions_counted(self):
        with tempfile.TemporaryDirectory() as d:
            a, b = Path(d) / "a.csv", Path(d) / "b.csv"
            first = baseline.generate(a, 105, 3, True)
            second = baseline.generate(b, 105, 3, True)
            self.assertEqual(first["sha256"], second["sha256"])
            self.assertEqual(first["zero_coordinate_rows"], 1)
            self.assertEqual(first["filtered_expected_rows"], 104)
            self.assertEqual(len(a.read_text().splitlines()), 107)
            with self.assertRaises(ValueError):
                baseline.generate(a, 105, 3, True)
            self.assertEqual(baseline.sha(a), first["sha256"])

    def test_run_directory_never_overwritten(self):
        with tempfile.TemporaryDirectory() as d:
            marker = Path(d) / "precious.txt"
            marker.write_text("keep")
            with self.assertRaises(FileExistsError):
                baseline.execute(["not-a-command"], d)
            self.assertEqual(marker.read_text(), "keep")

    def test_freeze_includes_assets_untracked_source_and_excludes_inputs(self):
        with tempfile.TemporaryDirectory() as d:
            repo=Path(d)/'repo';repo.mkdir()
            names=['internal/new.go','ui/logo.svg','packaging/icon.png','VERSION','LICENSE','inputs/scans.csv','.env','credentials.json']
            for name in names:
                p=repo/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_text('fixture')
            def git(_repo,*args):return '\0'.join(names) if args[0]=='ls-files' else 'test-head'
            with patch.object(baseline,'git',git):
                result=baseline.freeze(repo,Path(d)/'frozen')
            self.assertEqual({r['path'] for r in result['files']},{'internal/new.go','ui/logo.svg','packaging/icon.png','VERSION','LICENSE'})
            self.assertTrue(result['includes_untracked'])

    def test_memory_guard_stops_owned_probe_and_preserves_result(self):
        with tempfile.TemporaryDirectory() as d:
            out=Path(d)/'trial'
            result=baseline.execute([sys.executable,'-c','import time; time.sleep(10)'],out,max_rss_bytes=0)
            self.assertEqual(result['stopped_reason'],'memory ceiling exceeded')
            self.assertNotEqual(result['exit_code'],0)
            self.assertTrue((out/'result.json').is_file())


if __name__ == "__main__":
    unittest.main()
