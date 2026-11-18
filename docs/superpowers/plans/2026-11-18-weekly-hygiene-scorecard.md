# Weekly hygiene: OpenSSF Scorecard alerts — Plan

Spec: `docs/superpowers/specs/2026-11-18-weekly-hygiene-scorecard-design.md`.

1. ioc-scanning.yml: both PyYAML installs use the hash-locked requirements file.
2. semgrep.yml: generate `.github/requirements-semgrep.txt` with `uv pip compile --generate-hashes` (linux x86_64, Python 3.12); install with `--require-hashes`; add the version-drift check; key the pip cache on the file hash; add the file to the semgrep scan-relevance groups.
3. npm-install-if-lockfile-changed.sh: `npm ci`; remove the dead mutation warning; update its test.
4. concord-dev.sh: lock-stamp helper; `npm ci` replaces both `npm install` calls.
5. Validate: actionlint, workflow-validate checks, script tests, `bash -n`, a real `npm ci` stamp round-trip.
6. Adversarial verify pass over the diff.
7. Dismiss #498, #468, #469, #467 with recorded reasons; escalate #444.
