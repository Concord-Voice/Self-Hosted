"""Tiny offline checkout/upload boundary adapter for actual scanner workflow YAML.

This is not an Actions runner: it runs fixture Git checkouts, observes the HEAD
consumed by a scanner, and models only documented checkout/upload action defaults.
No scanner/install command, network request, action download or live upload runs.
"""
import copy
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from types import SimpleNamespace

import yaml

from test_ci_scan_verdict import MODULE, init_fixture_repo, commit_files, run_git

ROOT = Path(__file__).resolve().parents[3]
WORKFLOWS = ROOT / '.github/workflows'
_TOKEN = re.compile(r"\s*(?:(?P<string>'(?:[^']|'')*')|(?P<op>\&\&|\|\||==|!=|[!().,])|(?P<word>[A-Za-z_][A-Za-z_0-9-]*)|(?P<number>[0-9]+))")


class Expression:
    """Finite expression syntax used at the inspected action input boundaries."""
    def __init__(self, source, context):
        self.context = context
        self.tokens = []
        offset = 0
        source = source.strip()
        while offset < len(source):
            match = _TOKEN.match(source, offset)
            if match is None:
                raise AssertionError('unsupported workflow fixture expression: ' + source)
            self.tokens.append((match.lastgroup, match.group(match.lastgroup)))
            offset = match.end()
        self.index = 0

    def take(self, token):
        if self.index < len(self.tokens) and self.tokens[self.index][1] == token:
            self.index += 1
            return True
        return False

    def evaluate(self):
        result = self.binary(0)
        if self.index != len(self.tokens):
            raise AssertionError('unconsumed workflow fixture expression')
        return result

    def binary(self, level):
        operators = (('||',), ('&&',), ('==', '!='))
        if level == len(operators): return self.atom()
        value = self.binary(level + 1)
        while self.index < len(self.tokens) and self.tokens[self.index][1] in operators[level]:
            operator = self.tokens[self.index][1]
            self.index += 1
            right = self.binary(level + 1)
            if operator == '||': value = value or right
            elif operator == '&&': value = value and right
            elif operator == '==': value = value == right
            else: value = value != right
        return value

    def atom(self):
        if self.take('!'): return not self.atom()
        if self.take('('):
            value = self.binary(0)
            if not self.take(')'): raise AssertionError('unclosed fixture expression')
        else:
            if self.index >= len(self.tokens): raise AssertionError('incomplete fixture expression')
            kind, token = self.tokens[self.index]
            self.index += 1
            if kind == 'string': value = token[1:-1].replace("''", "'")
            elif kind == 'number': value = int(token)
            elif token in ('true', 'false', 'null'): value = {'true': True, 'false': False, 'null': None}[token]
            elif self.take('('):
                args = []
                if not self.take(')'):
                    while True:
                        args.append(self.binary(0))
                        if self.take(')'): break
                        if not self.take(','): raise AssertionError('invalid fixture function arguments')
                if token == 'fromJSON': value = json.loads(args[0])
                elif token == 'toJSON': value = json.dumps(args[0])
                elif token == 'format': value = args[0].format(*args[1:])
                elif token in ('always', 'success') and not args: value = True
                elif token in ('cancelled', 'failure') and not args: value = False
                else: raise AssertionError('unsupported workflow fixture function: ' + token)
            else: value = self.context.get(token, {})
        while self.take('.'):
            if self.index >= len(self.tokens): raise AssertionError('missing fixture property')
            name = self.tokens[self.index][1]
            self.index += 1
            value = value.get(name, '') if isinstance(value, dict) else ''
        return value


def resolve(value, context):
    if not isinstance(value, str): return value
    pattern = re.compile(r'\$\{\{(.*?)\}\}', re.S)
    match = pattern.fullmatch(value.strip())
    if match: return Expression(match[1], context).evaluate()
    return pattern.sub(lambda match: str(Expression(match[1], context).evaluate()), value)


def condition(value, context):
    if value is None: return True
    if '${{' in value: return bool(resolve(value, context))
    return bool(Expression(value, context).evaluate())


def workflow(name):
    return yaml.load((WORKFLOWS / name).read_text(), Loader=yaml.BaseLoader)


def twin_merges(root):
    init_fixture_repo(root)
    base = commit_files(root, 'base', {'fixture.txt': 'base\n'})
    head = commit_files(root, 'head', {'fixture.txt': 'base\nhead\n'})
    tree = run_git(root, 'rev-parse', head + '^{tree}')
    merges = []
    for date in ('2000-01-01T00:00:00Z', '2000-01-01T00:00:01Z'):
        env = dict(MODULE.GIT_ENV, GIT_AUTHOR_DATE=date, GIT_COMMITTER_DATE=date)
        result = subprocess.run(['git', '-C', str(root), 'commit-tree', tree, '-p', base, '-p', head,
                                 '-m', 'synthetic merge'], env=env, capture_output=True, text=True, check=True)
        merges.append(result.stdout.strip())
    return base, head, *merges


def admitted_metadata(repo, response):
    """Run the actual detector's metadata consumer against an offline GET stub."""
    step = next(step for step in workflow('pr-ci.yml')['jobs']['detect-changes']['steps']
                if step.get('id') == 'metadata')
    with tempfile.TemporaryDirectory() as directory:
        fixture = Path(directory)
        reply = fixture / 'pr.json'
        reply.write_text(json.dumps(response))
        calls = fixture / 'http-calls.jsonl'
        output = fixture / 'outputs'
        gh = fixture / 'gh'
        gh.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys
expected = ['api', 'repos/Concord-Voice/Self-Hosted/pulls/3691']
if sys.argv[1:] != expected:
    raise SystemExit('unexpected fixture HTTP request')
with pathlib.Path(os.environ['FIXTURE_HTTP_CALLS']).open('a') as stream:
    stream.write(json.dumps(sys.argv[1:]) + '\\n')
sys.stdout.write(pathlib.Path(os.environ['FIXTURE_HTTP_REPLY']).read_text())
""")
        gh.chmod(0o700)
        env = {'PATH': str(fixture) + os.pathsep + os.environ.get('PATH', ''),
               'LANG': 'C', 'LC_ALL': 'C', 'GITHUB_REPOSITORY': 'Concord-Voice/Self-Hosted',
               'PR_NUMBER': '3691', 'RUNNER_TEMP': str(fixture), 'GITHUB_OUTPUT': str(output),
               'FIXTURE_HTTP_REPLY': str(reply), 'FIXTURE_HTTP_CALLS': str(calls)}
        completed = subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', step['run']],
            cwd=repo, env=env, capture_output=True, text=True)
        if completed.returncode:
            raise AssertionError('actual metadata consumer failed: ' + completed.stderr)
        observations = [json.loads(line) for line in calls.read_text().splitlines()]
        if observations != [['api', 'repos/Concord-Voice/Self-Hosted/pulls/3691']]:
            raise AssertionError('metadata fixture must make exactly one bounded GET')
        return dict(line.split('=', 1) for line in output.read_text().splitlines())


def revision_step(step, context, repo, live_pr, merge_reply):
    """Execute the actual inline preflight with only exact offline GET routes."""
    with tempfile.TemporaryDirectory() as directory:
        fixture = Path(directory)
        responses = fixture / 'responses.json'
        calls = fixture / 'http-calls.jsonl'
        responses.write_text(json.dumps({
            'repos/Concord-Voice/Self-Hosted/pulls/3691': live_pr,
            'repos/Concord-Voice/Self-Hosted/commits/' + str(live_pr['merge_commit_sha']): merge_reply,
        }))
        gh = fixture / 'gh'
        gh.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys
with pathlib.Path(os.environ['FIXTURE_HTTP_CALLS']).open('a') as stream:
    stream.write(json.dumps(sys.argv[1:]) + '\\n')
responses = json.loads(pathlib.Path(os.environ['FIXTURE_HTTP_RESPONSES']).read_text())
if len(sys.argv) != 3 or sys.argv[1] != 'api' or sys.argv[2] not in responses:
    raise SystemExit('unexpected fixture HTTP request')
sys.stdout.write(json.dumps(responses[sys.argv[2]]))
""")
        gh.chmod(0o700)
        env = {'PATH': str(fixture) + os.pathsep + os.environ.get('PATH', ''),
               'LANG': 'C', 'LC_ALL': 'C', 'GITHUB_REPOSITORY': context['github']['repository'],
               'GITHUB_EVENT_NAME': context['github']['event_name'], 'RUNNER_TEMP': str(fixture),
               'GITHUB_OUTPUT': str(fixture / 'outputs'), 'FIXTURE_HTTP_RESPONSES': str(responses),
               'FIXTURE_HTTP_CALLS': str(calls)}
        for key, value in step.get('env', {}).items():
            resolved = resolve(value, context)
            env[key] = str(resolved).lower() if isinstance(resolved, bool) else str(resolved)
        completed = subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', step['run']],
            cwd=repo, env=env, capture_output=True, text=True)
        observed = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
        return SimpleNamespace(returncode=completed.returncode, stdout=completed.stdout,
                               stderr=completed.stderr, calls=observed)


class WorkflowConsumer:
    def __init__(self, repo, selection, event_merge, *, external=True, ordinary_main=False):
        self.repo = repo
        self.selection = selection
        self.event_merge = event_merge
        self.context = {
            'inputs': {'trusted-source': external, 'enforcement-only': external},
            'needs': {'detect-changes': {'result': 'success', 'outputs': {
                'scan-known': 'true', 'scan-selection': json.dumps(selection), 'protected-owner': 'true'}}},
            'github': {'sha': event_merge, 'ref': 'refs/heads/main' if ordinary_main else 'refs/pull/3691/merge',
                'event_name': 'push' if ordinary_main else 'pull_request', 'repository': 'Concord-Voice/Self-Hosted',
                'workflow_ref': 'Concord-Voice/Self-Hosted/.github/workflows/required-pr-build.yml@refs/heads/main',
                'workflow_sha': selection['policy_sha'], 'event': {'pull_request': {
                    'number': 3691, 'head': {'sha': selection['head_sha'], 'repo': {'full_name': 'Concord-Voice/Self-Hosted'}},
                    'base': {'sha': selection['base_sha'], 'ref': 'main'}}}},
            'job': {'workflow_repository': 'Concord-Voice/Self-Hosted', 'workflow_sha': selection['policy_sha']},
            'runner': {}, 'steps': {},
        }
        self.external = external
        self.ordinary_main = ordinary_main
        self.checkouts = []
        self.uploads = []
        self.preflights = []
        self.execution_boundaries = []
        event_pr = copy.deepcopy(self.context['github']['event']['pull_request'])
        self.live_pr = dict(event_pr, merge_commit_sha=selection['merge_sha'])
        parents = run_git(repo, 'show', '-s', '--format=%P', selection['merge_sha']).split()
        self.merge_reply = {'sha': selection['merge_sha'], 'parents': [{'sha': sha} for sha in parents]}

    def run_scanner(self, name, *, overrides=None, input_overrides=None):
        caller = workflow('pr-ci.yml')['jobs'][name]
        callee_name = caller['uses'].rsplit('/', 1)[-1]
        callee = copy.deepcopy(workflow(callee_name))
        if overrides: overrides(callee)
        definitions = callee.get('on', {}).get('workflow_call', {}).get('inputs', {})
        inputs = {}
        for key, definition in definitions.items():
            default = definition.get('default', '')
            inputs[key] = default == 'true' if definition.get('type') == 'boolean' else default
        if not self.ordinary_main:
            inputs.update({key: resolve(value, self.context) for key, value in caller.get('with', {}).items()})
        if input_overrides: inputs.update(input_overrides)
        context = copy.deepcopy(dict(self.context, inputs=inputs))
        for job in callee['jobs'].values():
            for step in job.get('steps', []):
                if step.get('id') == 'revision':
                    if condition(step.get('if'), context):
                        outcome = revision_step(step, context, self.repo, self.live_pr, self.merge_reply)
                        self.preflights.append((name, outcome))
                        context['steps']['revision'] = {'outcome': 'success' if outcome.returncode == 0 else 'failure'}
                        if outcome.returncode != 0: return False
                    else:
                        context['steps']['revision'] = {'outcome': 'skipped'}
                    continue
                uses = step.get('uses', '')
                if re.search(r'\b(?:npm ci|go install|pip install|pip3 install|semgrep scan|govulncheck -format)\b', step.get('run', '')):
                    if condition(step.get('if'), context): self.execution_boundaries.append((name, step.get('name', 'scanner command')))
                if not uses.startswith(('actions/checkout@', 'github/codeql-action/upload-sarif@')):
                    continue
                if not condition(step.get('if'), context): continue
                fields = {key: resolve(value, context) for key, value in step.get('with', {}).items()}
                if uses.startswith('actions/checkout@'):
                    # Documented default is the event ref/SHA, not the live PR API merge.
                    commit = fields.get('ref') or self.event_merge
                    run_git(self.repo, 'checkout', '-q', '--detach', commit)
                    actual = run_git(self.repo, 'rev-parse', 'HEAD')
                    self.checkouts.append((name, actual))
                elif uses.startswith('github/codeql-action/upload-sarif@'):
                    # The upstream pinned action defaults SHA/ref to GITHUB_SHA/REF
                    # independently of the checkout. Both inputs must be explicit.
                    if bool(fields.get('sha')) != bool(fields.get('ref')):
                        raise AssertionError('upload action requires explicit ref and sha together')
                    commit = fields.get('sha') or self.context['github']['sha']
                    ref = fields.get('ref') or self.context['github']['ref']
                    head_repo = self.context['github']['event']['pull_request']['head']['repo']['full_name']
                    if self.context['github']['event_name'] == 'pull_request' and head_repo != self.context['github']['repository']:
                        # Documented pinned upload-sarif behavior: fork overrides
                        # are ignored. This adapter does not grant upload authority.
                        commit, ref = self.context['github']['sha'], self.context['github']['ref']
                    self.uploads.append(dict(scanner=name, category=fields['category'],
                        commit_sha=commit, ref=ref, checked_out=run_git(self.repo, 'rev-parse', 'HEAD')))
