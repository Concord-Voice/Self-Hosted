"""Actual protected scanner preflight against bounded offline GitHub GET fixtures."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

from scan_revision_fixture import WorkflowConsumer, twin_merges, workflow
from test_ci_scan_verdict import commit_files, run_git, selection

SCANNERS = ('semgrep', 'eslint', 'govulncheck')
PRIVATE = 'synthetic-private-http-detail'


class ScanRevisionPreflightTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name)
        self.base, self.head, self.event, self.selected = twin_merges(self.repo)
        self.selection = selection(dict(semgrep_custom=True, semgrep_community=True, eslint=True, govulncheck=True))
        merge_base = run_git(self.repo, 'show', '-s', '--format=%P', self.selected).split()[0]
        self.selection.update(base_sha=self.base, head_sha=self.head, merge_sha=self.selected,
                              merge_base_sha=merge_base)

    def consumer(self, *, external=True):
        return WorkflowConsumer(self.repo, self.selection, self.event, external=external)

    def reject(self, scanner, consumer, inputs=None, calls=None):
        consumer.run_scanner(scanner, input_overrides=inputs)
        self.assertEqual(1, len(consumer.preflights), 'actual external preflight must run')
        outcome = consumer.preflights[0][1]
        self.assertNotEqual(0, outcome.returncode, 'untrusted revision reached checkout instead of preflight refusal')
        self.assertIn('Protected scan revision input or provenance invalid', outcome.stdout + outcome.stderr)
        self.assertNotIn(PRIVATE, outcome.stdout + outcome.stderr)
        self.assertEqual([], consumer.checkouts, 'preflight refusal must precede every checkout')
        self.assertEqual([], consumer.execution_boundaries, 'preflight refusal must precede install/scan boundaries')
        self.assertEqual([], consumer.uploads)
        if calls is not None: self.assertEqual(calls, len(outcome.calls))

    def test_positive_exact_api_tuple_accepts_event_twin_and_reaches_real_checkout(self):
        for scanner in SCANNERS:
            with self.subTest(scanner=scanner):
                consumer = self.consumer()
                consumer.run_scanner(scanner)
                self.assertEqual(1, len(consumer.preflights))
                outcome = consumer.preflights[0][1]
                self.assertEqual(0, outcome.returncode, outcome.stdout + outcome.stderr)
                self.assertEqual([
                    ['api', 'repos/Concord-Voice/Self-Hosted/pulls/3691'],
                    ['api', 'repos/Concord-Voice/Self-Hosted/commits/' + self.selected],
                    ['api', 'repos/Concord-Voice/Self-Hosted/git/ref/heads/main'],
                    ['api', 'repos/Concord-Voice/Self-Hosted/compare/' + self.selection['merge_base_sha'] + '...' + self.selection['merge_base_sha']],
                ], outcome.calls)
                self.assertEqual([(scanner, self.selected)], consumer.checkouts)
                self.assertTrue(consumer.execution_boundaries, 'positive control reaches actual declared scan/install boundaries')
                self.assertTrue(all(upload['commit_sha'] == self.selected for upload in consumer.uploads))

    def test_fresh_api_base_can_lag_protected_synthetic_merge_parent(self):
        """The API base remains an identity field; merge^1 is scan provenance.

        This runs the real external-workflow preflight.  It deliberately gives
        the API an older base while the synthetic merge has a later protected
        main parent, so the current failure is the old ordered-parent check,
        not an absent future merge-base input.
        """
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            base, head, _event, _selected = twin_merges(repo)
            run_git(repo, 'checkout', '-q', '--detach', base)
            merge_base = commit_files(repo, 'protected main advances', {'main-only.txt': 'trusted\n'})
            run_git(repo, 'update-ref', 'refs/heads/main', merge_base)
            run_git(repo, 'checkout', '-q', '--detach', merge_base)
            run_git(repo, 'merge', '-q', '--no-ff', '--no-edit', '-m', 'synthetic merge', head)
            synthetic_merge = run_git(repo, 'rev-parse', 'HEAD')

            selection = dict(self.selection, base_sha=base, head_sha=head, merge_sha=synthetic_merge,
                             merge_base_sha=merge_base)
            self.assertEqual([merge_base, head],
                             run_git(repo, 'show', '-s', '--format=%P', synthetic_merge).split())
            self.assertNotEqual(base, merge_base, 'fixture must model an API base that legitimately lags merge^1')

            for scanner in SCANNERS:
                with self.subTest(scanner=scanner):
                    consumer = WorkflowConsumer(repo, selection, synthetic_merge)
                    consumer.run_scanner(scanner)
                    self.assertEqual(1, len(consumer.preflights))
                    outcome = consumer.preflights[0][1]
                    self.assertEqual(0, outcome.returncode, outcome.stdout + outcome.stderr)
                    self.assertEqual([
                        ['api', 'repos/Concord-Voice/Self-Hosted/pulls/3691'],
                        ['api', 'repos/Concord-Voice/Self-Hosted/commits/' + synthetic_merge],
                        ['api', 'repos/Concord-Voice/Self-Hosted/git/ref/heads/main'],
                        ['api', 'repos/Concord-Voice/Self-Hosted/compare/' + merge_base + '...' + merge_base],
                    ], outcome.calls)
                    self.assertEqual([(scanner, synthetic_merge)], consumer.checkouts)

    def test_nonprotected_merge_parent_and_missing_main_proof_refuse_before_checkout(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            base, head, _event, _selected = twin_merges(repo)
            run_git(repo, 'checkout', '-q', '--detach', base)
            main = commit_files(repo, 'protected main advances', {'main-only.txt': 'trusted\n'})
            run_git(repo, 'update-ref', 'refs/heads/main', main)
            run_git(repo, 'checkout', '-q', '--detach', base)
            forged = commit_files(repo, 'unprotected parent', {'forged.txt': 'no trust\n'})
            run_git(repo, 'checkout', '-q', '--detach', forged)
            run_git(repo, 'merge', '-q', '--no-ff', '--no-edit', '-m', 'forged merge', head)
            forged_merge = run_git(repo, 'rev-parse', 'HEAD')
            selection = dict(self.selection, base_sha=base, head_sha=head, merge_sha=forged_merge,
                             merge_base_sha=forged)

            for scanner in SCANNERS:
                with self.subTest(scanner=scanner, case='nonprotected parent'):
                    self.reject(scanner, WorkflowConsumer(repo, selection, forged_merge), calls=4)
                with self.subTest(scanner=scanner, case='malformed protected main response'):
                    consumer = WorkflowConsumer(repo, selection, forged_merge)
                    consumer.main_reply = {'ref': 'refs/heads/main', 'object': {'type': 'tree', 'sha': main}}
                    self.reject(scanner, consumer, calls=3)

    def test_missing_or_malformed_selected_merge_base_refuses_before_http(self):
        for scanner in SCANNERS:
            for value in ('', 'not-a-sha', 'x' * 40):
                with self.subTest(scanner=scanner, value=value):
                    self.reject(scanner, self.consumer(), {'selected-merge-base-sha': value}, calls=0)

    def test_missing_malformed_and_unadmitted_tuples_refuse_without_checkout(self):
        variants = [
            {'selected-known': ''}, {'selected-known': 'false'}, {'selected-known': 'TRUE'},
            {'selected-pr-number': ''}, {'selected-pr-number': '03691'}, {'selected-pr-number': '3691;echo invalid'},
            {'selected-head-sha': ''}, {'selected-base-sha': ''}, {'selected-merge-sha': ''},
            {'selected-head-sha': 'not-a-sha'}, {'selected-base-sha': 'x' * 40}, {'selected-merge-sha': '../unadmitted'},
            {'selected-head-sha': self.base}, {'selected-base-sha': self.head},
        ]
        for scanner in SCANNERS:
            for inputs in variants:
                with self.subTest(scanner=scanner, inputs=inputs): self.reject(scanner, self.consumer(), inputs, calls=0)
            with self.subTest(scanner=scanner, case='valid-looking but unadmitted merge'):
                self.reject(scanner, self.consumer(), {'selected-merge-sha': self.event}, calls=1)

    def test_unknown_actual_detector_selection_cannot_fall_back_to_event_sha(self):
        for scanner in SCANNERS:
            for known, selected in (('false', '{}'), ('true', '{}'), ('false', json.dumps(self.selection))):
                consumer = self.consumer()
                outputs = consumer.context['needs']['detect-changes']['outputs']
                outputs.update({'scan-known': known, 'scan-selection': selected})
                with self.subTest(scanner=scanner, known=known, selection=selected):
                    self.reject(scanner, consumer, calls=0)

    def test_wrong_caller_repository_and_invalid_event_identity_refuse_before_http(self):
        for scanner in SCANNERS:
            for name in ('caller-ref', 'caller-sha', 'repository', 'event-kind', 'invalid-head-repo', 'base-ref', 'pr-number', 'head', 'base'):
                consumer = self.consumer()
                github = consumer.context['github']
                event = github['event']['pull_request']
                if name == 'caller-ref': github['workflow_ref'] = 'Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@refs/pull/3691/merge'
                elif name == 'caller-sha': github['workflow_sha'] = ''
                elif name == 'repository': github['repository'] = 'untrusted/other'
                elif name == 'event-kind': github['event_name'] = 'push'
                elif name == 'invalid-head-repo': event['head']['repo']['full_name'] = ''
                elif name == 'base-ref': event['base']['ref'] = 'unreviewed'
                elif name == 'pr-number': event['number'] = 42
                else: event[name]['sha'] = self.event
                # selected PR number is derived from the actual event in pr-ci;
                # pin its admitted value for the intentional event-number mismatch.
                inputs = {'selected-pr-number': '3691'} if name == 'pr-number' else None
                with self.subTest(scanner=scanner, name=name): self.reject(scanner, consumer, inputs, calls=0)

    def test_live_pr_movement_or_identity_mismatch_refuses_before_commit_get(self):
        for scanner in SCANNERS:
            for name in ('head', 'base', 'merge', 'number', 'head-repo', 'base-ref'):
                consumer = self.consumer()
                consumer.live_pr['private_detail'] = PRIVATE
                if name in ('head', 'base'): consumer.live_pr[name]['sha'] = self.event
                elif name == 'merge': consumer.live_pr['merge_commit_sha'] = self.event
                elif name == 'number': consumer.live_pr['number'] = 42
                elif name == 'head-repo': consumer.live_pr['head']['repo']['full_name'] = 'untrusted/fork'
                else: consumer.live_pr['base']['ref'] = 'unreviewed'
                with self.subTest(scanner=scanner, name=name): self.reject(scanner, consumer, calls=1)

    def test_selected_merge_identity_and_exact_ordered_parent_pair_are_required(self):
        for scanner in SCANNERS:
            for name in ('missing', 'one', 'reversed', 'third', 'wrong', 'wrong-commit'):
                consumer = self.consumer()
                if name == 'missing': consumer.merge_reply.pop('parents')
                elif name == 'one': consumer.merge_reply['parents'] = [{'sha': self.head}]
                elif name == 'reversed': consumer.merge_reply['parents'].reverse()
                elif name == 'third': consumer.merge_reply['parents'].append({'sha': self.event})
                elif name == 'wrong': consumer.merge_reply['parents'][0]['sha'] = self.event
                else:
                    # Route remains the requested commit, but returned identity differs.
                    # Keep the route keyed to the requested admitted identity below.
                    consumer.merge_reply['sha'] = self.event
                with self.subTest(scanner=scanner, name=name): self.reject(scanner, consumer, calls=2)

    def test_preflight_precedes_checkout_and_repo_code_for_all_actual_callees(self):
        for scanner in SCANNERS:
            caller = workflow('pr-ci.yml')['jobs'][scanner]
            callee = workflow(caller['uses'].rsplit('/', 1)[-1])
            for job in callee['jobs'].values():
                steps = job.get('steps', [])
                revision = next((index for index, step in enumerate(steps) if step.get('id') == 'revision'), None)
                self.assertIsNotNone(revision, 'actual external preflight is missing')
                first_checkout = next(index for index, step in enumerate(steps) if step.get('uses', '').startswith('actions/checkout@'))
                self.assertLess(revision, first_checkout)
            # Supplementary ordering inspection is paired with actual refused
            # preflight execution and observed zero checkout/install above.

    def test_ordinary_nonexternal_fork_callees_preserve_baseline_upload_behavior(self):
        for scanner in SCANNERS:
            consumer = self.consumer(external=False)
            consumer.context['github']['workflow_ref'] = 'untrusted/fork/.github/workflows/pr-ci.yml@refs/pull/3691/merge'
            consumer.context['github']['event']['pull_request']['head']['repo']['full_name'] = 'untrusted/fork'
            consumer.run_scanner(scanner)
            with self.subTest(scanner=scanner):
                self.assertEqual([], consumer.preflights)
                self.assertEqual([(scanner, self.event)], consumer.checkouts)
                self.assertTrue(consumer.execution_boundaries)
                if scanner == 'govulncheck':
                    # Baseline govulncheck uploads unconditionally; unlike the
                    # Semgrep/ESLint PR gate, ordinary mode must preserve this.
                    self.assertEqual(1, len(consumer.uploads))
                    self.assertEqual('govulncheck', consumer.uploads[0]['category'])
                    self.assertEqual(self.event, consumer.uploads[0]['commit_sha'])
                    self.assertEqual(self.event, consumer.uploads[0]['checked_out'])
                    self.assertEqual('refs/pull/3691/merge', consumer.uploads[0]['ref'])
                else:
                    self.assertEqual([], consumer.uploads, 'ordinary Semgrep/ESLint PR uploads remain suppressed')


    def test_protected_fork_with_exact_event_and_live_repo_passes_all_revision_checks(self):
        for fork in ('synthetic-contributor/fork-repo', 'contributor/.github', 'contributor/_repo', 'contributor/-repo'):
            for scanner in SCANNERS:
                consumer = self.consumer()
                consumer.context['github']['event']['pull_request']['head']['repo']['full_name'] = fork
                consumer.live_pr['head']['repo']['full_name'] = fork
                consumer.run_scanner(scanner)
                with self.subTest(scanner=scanner, fork=fork):
                    self.assertEqual(1, len(consumer.preflights))
                    outcome = consumer.preflights[0][1]
                    self.assertEqual(0, outcome.returncode, 'coherent protected fork must not be rejected as an untrusted caller')
                    self.assertEqual([
                        ['api', 'repos/Concord-Voice/Self-Hosted/pulls/3691'],
                        ['api', 'repos/Concord-Voice/Self-Hosted/commits/' + self.selected],
                        ['api', 'repos/Concord-Voice/Self-Hosted/git/ref/heads/main'],
                        ['api', 'repos/Concord-Voice/Self-Hosted/compare/' + self.selection['merge_base_sha'] +
                         '...' + self.selection['merge_base_sha']],
                    ], outcome.calls)
                    self.assertEqual([(scanner, self.selected)], consumer.checkouts)
                    self.assertTrue(consumer.execution_boundaries)
                    # The pinned upstream upload action ignores ref/sha overrides
                    # for forks. Our adapter models that documented limitation;
                    # this is preflight/checkout proof, not a live fork SARIF receipt.
                    for upload in consumer.uploads:
                        self.assertEqual(self.event, upload['commit_sha'])
                        self.assertEqual(self.selected, upload['checked_out'])

    def test_protected_fork_repo_mismatch_and_malformed_identity_refuse(self):
        for scanner in SCANNERS:
            consumer = self.consumer()
            consumer.context['github']['event']['pull_request']['head']['repo']['full_name'] = 'synthetic-contributor/fork-repo'
            consumer.live_pr['head']['repo']['full_name'] = 'different-contributor/fork-repo'
            with self.subTest(scanner=scanner, case='event/live fork mismatch'):
                self.reject(scanner, consumer, calls=1)
            for malformed in ('', '/repo', 'owner/', 'owner/repo/extra', 'owner repo', 'owner/repo\n', None, 42):
                consumer = self.consumer()
                consumer.context['github']['event']['pull_request']['head']['repo']['full_name'] = malformed
                consumer.live_pr['head']['repo']['full_name'] = malformed
                with self.subTest(scanner=scanner, malformed=malformed):
                    self.reject(scanner, consumer, calls=0)


if __name__ == '__main__':
    unittest.main()
