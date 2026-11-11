import json
from pathlib import Path
import unittest

from publish_evidence import Publisher


class EvidencePublicationTests(unittest.TestCase):
    def test_urls_and_source_escapes_remain_byte_identical(self):
        source = r'const url="https://example.test"; regex=/\w+\s/; text="line\n";'
        self.assertEqual(Publisher([], Path.cwd()).text(source), source)

    def test_private_names_and_paths_in_keys_commands_and_results(self):
        root = Path.cwd()/'example-checkout'
        private = str(root.parent/'inputs'/'private-drive-name.csv')
        source = {'command': ['probe', private], 'real_input_hashes': {private: 'abc'},
                  'filename': 'private-drive-name.csv', 'seconds': 12.3, 'failed': True}
        p = Publisher([source], root)
        text = json.dumps(p.report(source))
        self.assertNotIn('private-drive-name', text)
        self.assertIn('private-input-01.csv', text)
        self.assertEqual(p.report(source)['seconds'], 12.3)
        self.assertTrue(p.report(source)['failed'])
        self.assertEqual(p.report(p.report(source)), p.report(source))

    def test_series_are_explicit_summaries_without_altering_reported_rates(self):
        source = {'runs_ms': list(range(200)), 'p95_ms': 189, 'samples': [
            {'seconds': i, 'completed': max(0, i-3)} for i in range(100)]}
        result = Publisher([source], Path.cwd()).report(source)
        self.assertEqual(result['p95_ms'], 189)
        self.assertEqual(result['runs_ms_summary']['count'], 200)
        self.assertEqual(result['runs_ms_summary']['median'], 99.5)
        self.assertEqual(result['samples_summary']['first_completed_seconds'], 4)
        self.assertNotIn('runs_ms', result)


if __name__ == '__main__':
    unittest.main()
