#!/usr/bin/env python3
"""Hermetic artifact tests; synthetic identities never authorize publication."""
import copy
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'release' / 'selfhost-bundle.py'
VERSION = '0.3.0'
ROOT = f'concord-selfhost-{VERSION}'
ARCHIVE = f'{ROOT}.tar.gz'
ASSETS = (ARCHIVE, 'selfhost-manifest.json', 'SHA256SUMS')
EPOCH = 1728000000


def digest(data):
    return hashlib.sha256(data).hexdigest()


class SelfhostBundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='selfhost-bundle-test-')
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.tree = self.base / 'public-tree'
        (self.tree / 'bin').mkdir(parents=True)
        (self.tree / 'z.txt').write_bytes(b'public payload\n')
        (self.tree / 'bin/start').write_bytes(b'#!/bin/sh\nexit 0\n')
        (self.tree / 'z.txt').chmod(0o644)
        (self.tree / 'bin/start').chmod(0o755)
        self.out = self.base / 'assets'

    def cli(self, *args, umask=0o022):
        return subprocess.run([sys.executable, str(SCRIPT), *map(str, args)],
                              cwd=self.base, capture_output=True, text=True,
                              umask=umask, timeout=30)

    def build(self, out=None, *, version=VERSION, source='a' * 40, umask=0o022):
        return self.cli('build', '--public-tree', self.tree, '--source-sha', source,
                        '--version', version,
                        '--source-epoch', EPOCH, '--out', out or self.out, umask=umask)

    def built(self):
        result = self.build()
        self.assertEqual(result.returncode, 0, result.stderr)

    def verify(self):
        return self.cli('verify', '--archive', self.out / ARCHIVE,
                        '--manifest', self.out / 'selfhost-manifest.json',
                        '--sums', self.out / 'SHA256SUMS', '--public-tree', self.tree)

    def assert_refused(self, result, diagnostic):
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('Traceback', result.stderr)
        self.assertRegex(result.stderr.lower(), diagnostic)

    def refresh_sums(self):
        (self.out / 'SHA256SUMS').write_text(''.join(
            f'{digest((self.out / name).read_bytes())}  {name}\n'
            for name in sorted(ASSETS[:2])))

    def rewrite_archive(self, transform):
        """Change structure while retaining valid outer checksums."""
        with tarfile.open(self.out / ARCHIVE, 'r:gz') as source:
            entries = [(copy.copy(member), source.extractfile(member).read()
                        if member.isfile() else b'') for member in source.getmembers()]
        with tarfile.open(self.out / ARCHIVE, 'w:gz') as target:
            for member, data in transform(entries):
                target.addfile(member, io.BytesIO(data) if member.isfile() else None)
        self.refresh_sums()

    def rewrite_manifest(self, transform):
        manifest = json.loads((self.out / 'selfhost-manifest.json').read_bytes())
        transform(manifest)
        data = (json.dumps(manifest, sort_keys=True, indent=2) + '\n').encode()
        (self.out / 'selfhost-manifest.json').write_bytes(data)

        def replace(entries):
            for member, old in entries:
                if member.name == f'{ROOT}/selfhost-manifest.json':
                    member.size = len(data)
                    yield member, data
                else:
                    yield member, old
        self.rewrite_archive(replace)

    def test_determinism_across_paths_umasks_and_nonexecute_permissions(self):
        outputs = (self.base / 'first', self.base / 'elsewhere/second')
        for out, mask in zip(outputs, (0o022, 0o002)):
            (self.tree / 'z.txt').chmod(0o666 & ~mask)
            (self.tree / 'bin/start').chmod(0o777 & ~mask)
            result = self.build(out, umask=mask)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(sorted(p.name for p in out.iterdir()), sorted(ASSETS))
        self.assertEqual({n: (outputs[0] / n).read_bytes() for n in ASSETS},
                         {n: (outputs[1] / n).read_bytes() for n in ASSETS})

    def test_inventory_metadata_and_checksums_bind_actual_bytes(self):
        self.built()
        data = (self.out / 'selfhost-manifest.json').read_bytes()
        manifest = json.loads(data)
        self.assertEqual(set(manifest), {'schema', 'tag', 'version', 'source_sha',
                                        'root', 'profile', 'files'})
        self.assertEqual({k: manifest[k] for k in manifest if k != 'files'}, {
            'schema': 'selfhost-bundle/v2', 'tag': f'v{VERSION}',
            'version': VERSION, 'source_sha': 'a' * 40,
            'root': ROOT, 'profile': 'services'})
        expected = {n: digest((self.tree / n).read_bytes()) for n in ('bin/start', 'z.txt')}
        self.assertEqual(manifest['files'], expected)
        self.assertEqual(list(manifest['files']), sorted(expected))
        self.assertNotIn('selfhost-manifest.json', manifest['files'])
        self.assertEqual((self.out / 'SHA256SUMS').read_text(), ''.join(
            f'{digest((self.out / n).read_bytes())}  {n}\n' for n in sorted(ASSETS[:2])))
        header = (self.out / ARCHIVE).read_bytes()[:10]
        self.assertEqual(header[3] & 0x08, 0, 'no gzip filename')
        self.assertEqual(header[4:8], b'\0' * 4, 'fixed gzip time')
        with tarfile.open(self.out / ARCHIVE, 'r:gz') as archive:
            members = archive.getmembers()
            self.assertEqual([m.name for m in members], sorted(m.name for m in members))
            self.assertEqual({m.name for m in members if m.isfile()},
                             {f'{ROOT}/{n}' for n in (*expected, 'selfhost-manifest.json')})
            self.assertEqual(archive.extractfile(f'{ROOT}/selfhost-manifest.json').read(), data)
            for member in members:
                self.assertEqual((member.uid, member.gid, member.uname, member.gname),
                                 (0, 0, '', ''))
                self.assertEqual(member.mtime, EPOCH)
                mode = 0o755 if member.isdir() or member.name.endswith('/bin/start') else 0o644
                self.assertEqual(member.mode, mode)
                if member.isfile() and member.name != f'{ROOT}/selfhost-manifest.json':
                    relative = member.name.removeprefix(f'{ROOT}/')
                    self.assertEqual(archive.extractfile(member).read(),
                                     (self.tree / relative).read_bytes())
        result = self.verify()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_changed_staged_payload_is_refused(self):
        self.built()
        (self.tree / 'z.txt').write_bytes(b'changed staged bytes\n')
        self.assert_refused(self.verify(), r'payload|snapshot|public.tree')

    def test_changed_archive_payload_is_refused_with_valid_outer_checksums(self):
        self.built()

        def change(entries):
            for member, data in entries:
                if member.name == f'{ROOT}/z.txt':
                    data = b'changed archive bytes\n'
                    member.size = len(data)
                yield member, data
        self.rewrite_archive(change)
        self.assert_refused(self.verify(), r'payload|digest|hash')

    def test_unknown_manifest_schema_is_refused(self):
        self.built()
        self.rewrite_manifest(lambda m: m.update(schema='selfhost-bundle/v99'))
        self.assert_refused(self.verify(), r'schema')

    def test_manifest_cannot_inventory_itself(self):
        self.built()
        self.rewrite_manifest(lambda m: m['files'].update({'selfhost-manifest.json': '0' * 64}))
        self.assert_refused(self.verify(), r'manifest|inventory|payload')

    def test_noncanonical_versions_and_nonfull_identities_are_refused(self):
        for version in ('0.2.47-rc1', '01.2.47', '0.2', 'v0.2.47'):
            with self.subTest(version=version):
                self.assert_refused(self.build(version=version), r'version')
        for value in ('a' * 39, 'A' * 40, 'g' * 40):
            with self.subTest(value=value):
                self.assert_refused(self.build(source=value), r'sha|identity')

    def test_bad_manifest_version_is_refused_with_consistent_hashes(self):
        self.built()
        self.rewrite_manifest(lambda m: m.update(version='0.2.47-rc1'))
        self.assert_refused(self.verify(), r'version')

    def test_standalone_manifest_must_equal_inside_copy(self):
        self.built()
        path = self.out / 'selfhost-manifest.json'
        path.write_bytes(path.read_bytes() + b'\n')
        self.refresh_sums()
        self.assert_refused(self.verify(), r'manifest')

    def test_checksum_tampering_is_refused(self):
        self.built()
        path = self.out / 'SHA256SUMS'
        path.write_text(path.read_text().replace(digest((self.out / ARCHIVE).read_bytes()), '0' * 64))
        self.assert_refused(self.verify(), r'checksum|digest|sha256')

    def test_unsafe_members_refused_with_valid_outer_checksums_before_writes(self):
        for case in ('traversal', 'absolute', 'symlink', 'device', 'duplicate', 'mode'):
            with self.subTest(case=case):
                self.built()

                def forge(entries):
                    index = next(i for i, (m, _) in enumerate(entries) if m.name == f'{ROOT}/z.txt')
                    member, data = entries[index]
                    if case == 'traversal':
                        member.name = f'{ROOT}/../escape'
                    elif case == 'absolute':
                        member.name = str(self.base / 'escape')
                    elif case == 'symlink':
                        member.type, member.linkname, member.size = tarfile.SYMTYPE, '../../escape', 0
                    elif case == 'device':
                        member.type, member.size = tarfile.CHRTYPE, 0
                        member.devmajor, member.devminor = 1, 3
                    elif case == 'duplicate':
                        entries.append((copy.copy(member), data))
                    elif case == 'mode':
                        member.mode = 0o755
                    return entries

                self.rewrite_archive(forge)
                before = {str(p.relative_to(self.base)): p.read_bytes()
                          for p in self.base.rglob('*') if p.is_file()}
                diagnostic = {'traversal': r'path|traversal', 'absolute': r'path|absolute',
                              'symlink': r'link|type', 'device': r'device|type',
                              'duplicate': r'duplicate', 'mode': r'mode|permission'}[case]
                self.assert_refused(self.verify(), diagnostic)
                after = {str(p.relative_to(self.base)): p.read_bytes()
                         for p in self.base.rglob('*') if p.is_file()}
                self.assertEqual(after, before, 'refusal precedes extraction/writes')
                self.assertFalse((self.base / 'escape').exists())


if __name__ == '__main__':
    unittest.main()
