from contextlib import closing
import hashlib
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

import memory_preparation as m

class SnapshotSafety(unittest.TestCase):
    def source(self,root):
        source=root/'source';(source/'observations').mkdir(parents=True)
        with closing(sqlite3.connect(source/'atlas.sqlite3')) as c:
            c.executescript("CREATE TABLE imports(active_rows);INSERT INTO imports VALUES(1);CREATE TABLE observations(id);")
        (source/'observations/manifest.json').write_text(json.dumps({'strings':1,'segments':[{'file':'seg-owned.bin','n':1}]}))
        (source/'observations/strings.bin').write_bytes(bytes([1,88]))
        (source/'observations/seg-owned.bin').write_bytes(b'immutable source')
        return source

    def test_snapshot_only_changes_the_independent_copy(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);source=self.source(root)
            digest=lambda:hashlib.sha256((source/'atlas.sqlite3').read_bytes()).hexdigest()
            before=digest();m.snapshot_source(source,root/'copy')
            with closing(sqlite3.connect(root/'copy/atlas.sqlite3')) as c:
                c.execute('UPDATE imports SET active_rows=0');c.commit()
            (root/'copy/observations/strings.bin').write_bytes(b'changed copy')
            self.assertEqual(before,digest())
            self.assertEqual((source/'observations/strings.bin').read_bytes(),bytes([1,88]))
            self.assertEqual((root/'copy/observations/seg-owned.bin').read_bytes(),b'immutable source')

    def test_snapshot_rejects_traversal_before_copying(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);source=self.source(root)
            (source/'observations/manifest.json').write_text(json.dumps({'strings':1,'segments':[{'file':'../outside.bin','n':1}]}))
            with self.assertRaises(ValueError):m.snapshot_source(source,root/'copy')
            self.assertFalse((root/'copy').exists())

    def test_snapshot_never_overwrites_an_existing_destination(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);source=self.source(root);dest=root/'copy';dest.mkdir();(dest/'sentinel').write_text('keep')
            with self.assertRaises(ValueError):m.snapshot_source(source,dest)
            self.assertEqual((dest/'sentinel').read_text(),'keep')

if __name__=='__main__':unittest.main()
