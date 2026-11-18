#!/usr/bin/env bash
# Runs the descriptor-lifetime tests for the real voice-boot-containment.py.
set -euo pipefail
python3 "$(dirname "${BASH_SOURCE[0]}")/test-voice-boot-containment.py" -v
