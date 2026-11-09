"""Regression for Self-Hosted#147 run41367890055: event/API merge identity split."""
import copy
from pathlib import Path
import tempfile
import unittest

from scan_revision_fixture import WorkflowConsumer, twin_merges, admitted_metadata, workflow
from test_ci_scan_verdict import MODULE, PR, selection, analysis, evidence_for, run_git


class ScanRevisionBindingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name)
        self.base, self.head, self.event_merge, self.admitted_merge = twin_merges(self.repo)
        self.selection = selection(dict(semgrep_custom=True, semgrep_community=True, eslint=True, govulncheck=True))
        merge_base = run_git(self.repo, 'show', '-s', '--format=%P', self.admitted_merge).split()[0]
        self.selection.update(base_sha=self.base, head_sha=self.head, merge_sha=self.admitted_merge,
                              merge_base_sha=merge_base)
        self.pr = copy.deepcopy(PR)
        self.pr['head']['sha'] = self.head
        self.pr['head']['repo']['full_name'] = 'Concord-Voice/Self-Hosted'
        self.pr['base']['sha'] = self.base
        self.pr['merge_commit_sha'] = self.admitted_merge
        self.pr['labels'] = []
        self.pr['changed_files'] = 42
        admitted = admitted_metadata(self.repo, self.pr)
        self.assertEqual('true', admitted['valid'])
        self.assertEqual(self.head, admitted['head_sha'])
        self.assertEqual(self.base, admitted['base_sha'])
        self.assertEqual(self.admitted_merge, admitted['merge_sha'])
        self.assertEqual(merge_base, admitted['merge_base_sha'])
        self.selection.update(head_sha=admitted['head_sha'], base_sha=admitted['base_sha'],
                              merge_sha=admitted['merge_sha'], merge_base_sha=merge_base)
        self.assertNotEqual(self.event_merge, self.admitted_merge)
        self.assertEqual(run_git(self.repo, 'show', '-s', '--format=%P', self.event_merge),
                         run_git(self.repo, 'show', '-s', '--format=%P', self.admitted_merge))
        self.assertEqual(run_git(self.repo, 'rev-parse', self.event_merge + '^{tree}'),
                         run_git(self.repo, 'rev-parse', self.admitted_merge + '^{tree}'))

    def observed(self, *, ordinary_main=False, override=None):
        consumer = WorkflowConsumer(self.repo, self.selection, self.event_merge, ordinary_main=ordinary_main)
        for scanner in ('semgrep', 'eslint', 'govulncheck'):
            if ordinary_main and scanner == 'semgrep': continue  # no ordinary main Semgrep baseline contract
            consumer.run_scanner(scanner, overrides=override)
        return consumer

    def normalized(self, uploads):
        items = []
        for index, upload in enumerate(uploads):
            tool = {'semgrep': 'Semgrep OSS', 'eslint': 'ESLint', 'govulncheck': 'govulncheck'}[upload['scanner']]
            items.append(analysis(tool, upload['category'], aid=11 + index, sarif_id='upload-' + str(index),
                ref=upload['ref'], commit=upload['commit_sha']))
        self.assertEqual(4, len(items), 'all actual four upload actions must execute')
        evidence = evidence_for(items)
        evidence['run']['head_sha'] = self.head
        reports = {}
        for index, item in enumerate(items):
            own = evidence_for([item])['reports']
            for name, wrapper in own.items():
                if name in reports:
                    reports[name]['report']['uploads'].extend(wrapper['report']['uploads'])
                else:
                    wrapper['artifact']['id'] = 501 + index
                    reports[name] = wrapper
        evidence['reports'] = reports
        return evidence

    def verdict(self, evidence, *, pr=None):
        return MODULE.verify_verdict(self.selection,
            {'build': 'success', 'semgrep': 'success', 'eslint': 'success', 'govulncheck': 'success'},
            evidence, self.pr if pr is None else pr)

    def test_each_actual_scanner_checkout_consumes_admitted_merge_not_event_twin(self):
        consumer = self.observed()
        self.assertEqual(3, len(consumer.checkouts))
        for scanner, actual in consumer.checkouts:
            with self.subTest(scanner=scanner):
                self.assertEqual(self.admitted_merge, actual,
                    scanner + ' scanned event merge instead of admitted selected merge')

    def test_external_scanners_receive_distinct_api_and_merge_base_identities(self):
        """The protected selector must not replace API base identity with merge^1."""
        for scanner in ('semgrep', 'eslint', 'govulncheck'):
            with self.subTest(scanner=scanner):
                inputs = workflow('pr-ci.yml')['jobs'][scanner]['with']
                self.assertIn('selected-base-sha', inputs)
                self.assertIn("scan-selection || '{}').base_sha", inputs['selected-base-sha'])
                self.assertIn('selected-merge-base-sha', inputs)
                self.assertIn("scan-selection || '{}').merge_base_sha", inputs['selected-merge-base-sha'])

    def test_all_four_actual_uploads_bind_selected_merge_and_strict_verdict_accepts(self):
        consumer = self.observed()
        self.assertEqual({'semgrep-custom', 'semgrep-community', 'eslint', 'govulncheck'},
                         {upload['category'] for upload in consumer.uploads})
        for upload in consumer.uploads:
            with self.subTest(category=upload['category']):
                self.assertEqual(self.admitted_merge, upload['commit_sha'],
                    upload['category'] + ' upload inherited event SHA instead of selected merge')
                self.assertEqual('refs/pull/3691/merge', upload['ref'])
                self.assertEqual(self.admitted_merge, upload['checked_out'], 'upload must describe actually scanned revision')
        self.assertEqual([], self.verdict(self.normalized(consumer.uploads)))

    def test_fixture_normalization_positive_and_verifier_rejects_same_tree_other_object(self):
        # Positive control for real verifier fixture construction, independent of
        # the workflow regression, so malformed fake evidence cannot count as RED.
        uploads = [dict(scanner='semgrep' if category.startswith('semgrep') else category,
            category=category, commit_sha=self.admitted_merge, ref='refs/pull/3691/merge')
            for category in ('semgrep-custom', 'semgrep-community', 'eslint', 'govulncheck')]
        valid = self.normalized(uploads)
        self.assertEqual([], self.verdict(valid))
        for index in range(4):
            wrong = copy.deepcopy(valid)
            wrong['analyses'][index]['commit_sha'] = self.event_merge
            wrong['latest_analyses'][index]['commit_sha'] = self.event_merge
            with self.subTest(category=uploads[index]['category']):
                self.assertTrue(any('wrong revision' in reason for reason in self.verdict(wrong)))

    def test_current_head_base_merge_and_provenance_drift_are_still_refused(self):
        uploads = [dict(scanner='semgrep' if category.startswith('semgrep') else category,
            category=category, commit_sha=self.admitted_merge, ref='refs/pull/3691/merge')
            for category in ('semgrep-custom', 'semgrep-community', 'eslint', 'govulncheck')]
        evidence = self.normalized(uploads)
        self.assertEqual([], self.verdict(evidence))
        for field in ('head', 'base', 'merge'):
            changed = copy.deepcopy(self.pr)
            if field == 'merge': changed['merge_commit_sha'] = self.event_merge
            else: changed[field]['sha'] = self.event_merge
            with self.subTest(field=field):
                self.assertIn('unknown: PR ' + field + ' revision moved', self.verdict(evidence, pr=changed))
        for field in ('provenance_verified', 'artifact_verified'):
            changed = copy.deepcopy(evidence)
            changed[field] = False
            with self.subTest(field=field): self.assertTrue(self.verdict(changed))
        for key, value in (('ref', 'refs/heads/main'), ('sarif_id', 'old-upload')):
            changed = copy.deepcopy(evidence)
            changed['analyses'][0][key] = value
            with self.subTest(key=key): self.assertTrue(self.verdict(changed))

    def test_checkout_and_upload_adapter_controls_have_independent_identity_boundaries(self):
        def bound_actions(callee):
            for job in callee['jobs'].values():
                for step in job.get('steps', []):
                    uses = step.get('uses', '')
                    if uses.startswith('actions/checkout@'):
                        step.setdefault('with', {})['ref'] = self.admitted_merge
                    elif uses.startswith('github/codeql-action/upload-sarif@'):
                        step['with'].update(sha=self.admitted_merge, ref='refs/pull/3691/merge')
        control = self.observed(override=bound_actions)
        self.assertTrue(all(actual == self.admitted_merge for scanner, actual in control.checkouts))
        self.assertEqual([], self.verdict(self.normalized(control.uploads)))
        def event_upload(callee):
            bound_actions(callee)
            for job in callee['jobs'].values():
                for step in job.get('steps', []):
                    if step.get('uses', '').startswith('github/codeql-action/upload-sarif@'):
                        step['with'].pop('sha')
                        step['with'].pop('ref')
        wrong_upload = self.observed(override=event_upload)
        self.assertTrue(all(actual == self.admitted_merge for scanner, actual in wrong_upload.checkouts))
        self.assertEqual(4, sum('wrong revision' in reason for reason in self.verdict(self.normalized(wrong_upload.uploads))))
        def only_label_upload(callee):
            bound_actions(callee)
            for job in callee['jobs'].values():
                for step in job.get('steps', []):
                    if step.get('uses', '').startswith('actions/checkout@'):
                        step['with'].pop('ref')
        wrong_checkout = self.observed(override=only_label_upload)
        self.assertTrue(all(upload['commit_sha'] == self.admitted_merge for upload in wrong_checkout.uploads))
        self.assertTrue(all(upload['checked_out'] == self.event_merge for upload in wrong_checkout.uploads),
                        'explicit upload label alone cannot establish the scanned revision')

    def test_ordinary_nonexternal_main_callees_keep_event_checkout_and_upload_defaults(self):
        consumer = self.observed(ordinary_main=True)
        self.assertEqual([('eslint', self.event_merge), ('govulncheck', self.event_merge)], consumer.checkouts)
        self.assertEqual({'eslint', 'govulncheck'}, {upload['category'] for upload in consumer.uploads})
        for upload in consumer.uploads:
            self.assertEqual(self.event_merge, upload['commit_sha'])
            self.assertEqual(self.event_merge, upload['checked_out'])
            self.assertEqual('refs/heads/main', upload['ref'])


if __name__ == '__main__':
    unittest.main()
