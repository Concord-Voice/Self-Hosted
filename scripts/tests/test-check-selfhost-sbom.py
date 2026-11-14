#!/usr/bin/env python3
"""Tests for the self-host SBOM check and its release.yml wiring (#273)."""
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

import yaml

REPO = Path(__file__).resolve().parents[2]
CHECK = REPO / 'scripts' / 'release' / 'check-selfhost-sbom.py'
RELEASE = REPO / '.github' / 'workflows' / 'release.yml'

LOCK = {'lockfileVersion': 3, 'packages': {
    '': {'name': 'r'},
    'node_modules/left-pad': {'version': '1.3.0'},
    'node_modules/@scope/dev-only': {'version': '2.0.0', 'dev': True},
    'node_modules/alias': {'name': 'real-pkg', 'version': '3.1.0'},
    'node_modules/ws': {'link': True, 'resolved': 'packages/ws'},
}}
GOMOD = '''module example.com/m

go 1.24

require example.com/direct v1.2.3

require (
\texample.com/indirect v0.4.0 // indirect
)
'''
PURLS = ['pkg:npm/left-pad@1.3.0', 'pkg:npm/%40scope/dev-only@2.0.0', 'pkg:npm/real-pkg@3.1.0',
         'pkg:golang/example.com/direct@v1.2.3', 'pkg:golang/example.com/indirect@v0.4.0']


class CheckTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix='sbom-check-')
        self.addCleanup(temp.cleanup)
        self.base = Path(temp.name)
        self.tree = self.base / 'tree'
        (self.tree / 'svc').mkdir(parents=True)
        (self.tree / 'package-lock.json').write_text(json.dumps(LOCK))
        (self.tree / 'svc' / 'go.mod').write_text(GOMOD)

    def run_check(self, purls, fmt='CycloneDX', name='concord-selfhost', version='1.0.0', extra=''):
        sbom = self.base / 'x.cdx.json'
        sbom.write_text(json.dumps({'bomFormat': fmt, 'metadata': {'component': {'name': name, 'version': version}},
                                    'components': [{'purl': p} for p in purls]}) + extra)
        r = subprocess.run([sys.executable, str(CHECK), '--sbom', str(sbom), '--tree', str(self.tree),
                            '--version', '1.0.0'], capture_output=True, text=True)
        return r.returncode, r.stdout

    def test_complete_sbom_passes(self):
        rc, out = self.run_check(PURLS)
        self.assertEqual(rc, 0, out)
        self.assertIn('all 5 locked', out)

    def test_missing_dev_dependency_fails(self):
        rc, out = self.run_check([p for p in PURLS if 'dev-only' not in p])
        self.assertEqual(rc, 1)
        self.assertIn('missing npm @scope/dev-only@2.0.0', out)

    def test_missing_indirect_go_module_fails(self):
        rc, out = self.run_check([p for p in PURLS if 'indirect' not in p])
        self.assertEqual(rc, 1)
        self.assertIn('missing golang example.com/indirect@v0.4.0', out)

    def test_wrong_version_fails(self):
        rc, out = self.run_check([p.replace('1.3.0', '1.2.0') for p in PURLS])
        self.assertEqual(rc, 1)
        self.assertIn('left-pad@1.3.0', out)

    def test_not_cyclonedx_fails(self):
        rc, out = self.run_check(PURLS, fmt='SPDX')
        self.assertEqual(rc, 1)
        self.assertIn('not a CycloneDX document', out)

    def test_wrong_metadata_fails(self):
        self.assertEqual(self.run_check(PURLS, version='0.9.0')[0], 1)
        self.assertEqual(self.run_check(PURLS, name='other')[0], 1)

    def test_trailing_json_value_fails(self):
        rc, out = self.run_check(PURLS, extra='{"bomFormat": "CycloneDX"}')
        self.assertEqual(rc, 1)
        self.assertIn('cannot check', out)

    def test_require_paren_without_space_is_read(self):
        (self.tree / 'svc' / 'go.mod').write_text(GOMOD.replace('require (', 'require('))
        rc, out = self.run_check([p for p in PURLS if 'indirect' not in p])
        self.assertEqual(rc, 1)
        self.assertIn('example.com/indirect', out)

    def test_replace_fails_closed(self):
        (self.tree / 'svc' / 'go.mod').write_text(GOMOD + 'replace example.com/direct => example.com/fork v1.0.0\n')
        rc, out = self.run_check(PURLS)
        self.assertEqual(rc, 1)
        self.assertIn('replace is not modelled', out)

    def test_lockfile_v1_fails_closed(self):
        (self.tree / 'package-lock.json').write_text(json.dumps({'lockfileVersion': 1, 'dependencies': {}}))
        rc, out = self.run_check(PURLS)
        self.assertEqual(rc, 1)
        self.assertIn('lockfile version 1', out)

    def test_tree_without_go_fails(self):
        (self.tree / 'svc' / 'go.mod').unlink()
        rc, out = self.run_check(PURLS)
        self.assertEqual(rc, 1)
        self.assertIn('locks no golang', out)


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.jobs = yaml.safe_load(RELEASE.read_text())['jobs']

    def steps(self, job):
        return self.jobs[job]['steps']

    def test_sbom_job_has_no_id_token(self):
        self.assertEqual(self.jobs['sbom']['permissions'], {'contents': 'read'})

    def test_syft_is_pinned_by_version_and_checksum(self):
        env = self.jobs['sbom']['env']
        self.assertRegex(str(env['SYFT_VERSION']), r'^\d+\.\d+\.\d+$')
        self.assertRegex(env['SYFT_SHA256'], r'^[0-9a-f]{64}$')
        install = next(s['run'] for s in self.steps('sbom') if s['name'].startswith('Install syft'))
        self.assertIn('sha256sum -c -', install)

    def test_sbom_is_checked_and_uploaded_without_a_tag_suffix(self):
        names = [s['name'] for s in self.steps('sbom')]
        self.assertIn('Check the SBOM lists every locked package', names)
        upload = next(s for s in self.steps('sbom') if s['name'] == 'Upload the SBOM')
        # Release-Ops treats an artifact named *-<tag> as a release artifact.
        self.assertFalse(re.search(r'-v\$\{\{|-v\d', upload['with']['name']))
        self.assertTrue(upload['with']['path'].endswith('.cdx.json'))

    def test_bundle_job_adds_and_attests_the_sbom(self):
        self.assertIn('sbom', self.jobs['bundle']['needs'])
        self.assertIn('needs.sbom.result', self.jobs['bundle']['if'])
        names = [s['name'] for s in self.steps('bundle')]
        self.assertLess(names.index('Add the SBOM'), names.index('Upload the bundle'))
        # The untrusted artifact lands outside the attested set, and only the
        # one expected file is copied in.
        dl = next(s for s in self.steps('bundle') if s['name'] == 'Download the SBOM')
        self.assertNotIn('assets', dl['with']['path'])
        add = next(s['run'] for s in self.steps('bundle') if s['name'] == 'Add the SBOM')
        self.assertIn('"f $name"', add)
        self.assertIn('cp "$RUNNER_TEMP/sbom-in/$name" "$RUNNER_TEMP/assets/$name"', add)
        self.assertLess(add.index('check-selfhost-sbom.py'), add.index('cp "$RUNNER_TEMP'))
        gen = next(s['run'] for s in self.steps('sbom') if s['name'] == 'Generate the SBOM')
        self.assertIn('export HOME="$RUNNER_TEMP/syft-home"', gen)
        attest = next(s for s in self.steps('bundle') if s['name'] == 'Attest build provenance')
        self.assertIn('concord-selfhost-v*.cdx.json', attest['with']['subject-path'])

    def test_sbom_name_matches_the_release_artifact(self):
        gen = next(s['run'] for s in self.steps('sbom') if s['name'] == 'Generate the SBOM')
        upload = next(s for s in self.steps('bundle') if s['name'] == 'Upload the bundle')
        self.assertEqual(upload['with']['name'], 'concord-selfhost-v${{ needs.check-tag.outputs.version }}')
        self.assertIn('concord-selfhost-v${VERSION}.cdx.json', gen)


if __name__ == '__main__':
    unittest.main(verbosity=2)
