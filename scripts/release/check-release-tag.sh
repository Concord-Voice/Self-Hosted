#!/usr/bin/env bash
# Refuse a release tag unless it is v<VERSION> and its commit is on main.
# VERSION is one X.Y.Z line at the repo root; surrounding whitespace is ignored.
# Reads GITHUB_REF_NAME, GITHUB_SHA, GITHUB_OUTPUT. Run from the repo root.
set -euo pipefail

VERSION=$(tr -d '[:space:]' < VERSION)
if [[ ! "$VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "::error::VERSION must be one X.Y.Z line, got '${VERSION}'"
  exit 1
fi
if [[ "$GITHUB_REF_NAME" != "v${VERSION}" ]]; then
  echo "::error::tag ${GITHUB_REF_NAME} does not match VERSION ${VERSION}"
  exit 1
fi
git fetch --no-tags origin main
if ! git merge-base --is-ancestor "$GITHUB_SHA" origin/main; then
  echo "::error::${GITHUB_SHA} is not on main"
  exit 1
fi
echo "version=${VERSION}" >> "$GITHUB_OUTPUT"
