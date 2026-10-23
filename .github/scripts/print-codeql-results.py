#!/usr/bin/env python3
"""Read generated SARIF and emit only validated finding metadata."""
import json
import math
from pathlib import Path
import re
import sys
from urllib.parse import unquote

MAX_FILES = 100
MAX_RESULTS = 1000
MAX_BYTES = 10 * 1024 * 1024


def _require(condition):
    if not condition:
        raise ValueError('Invalid CodeQL metadata')


def _identifier(value):
    _require(isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9_-]+(?:/[A-Za-z0-9_.-]+)*', value))
    _require(all(part not in {'.', '..'} for part in value.split('/')))
    return value


def _path(uri):
    _require(isinstance(uri, str) and not re.search(r'%(?:2f|5c|2e)', uri, re.I))
    _require(not re.search(r'%(?![0-9a-fA-F]{2})', uri))
    value = unquote(uri, errors='strict')
    _require(value and not value.startswith('/') and not any(char in value for char in '\\:?#%'))
    _require(not any(ord(char) < 32 or ord(char) == 127 for char in value))
    _require(all(part not in {'', '.', '..'} for part in value.split('/')))
    return value


def _row(result, rules):
    _require(isinstance(result, dict))
    rule = None
    rule_id = result.get('ruleId')
    if 'ruleId' in result:
        rule_id = _identifier(rule_id)
    if 'ruleIndex' in result:
        index = result['ruleIndex']
        _require(type(index) is int and 0 <= index < len(rules))
        rule = rules[index]
        _require(isinstance(rule, dict))
        indexed_id = _identifier(rule.get('id'))
        _require(rule_id is None or rule_id == indexed_id)
        rule_id = indexed_id
    rule_id = _identifier(rule_id)
    if rule is None:
        matching = [entry for entry in rules if isinstance(entry, dict) and entry.get('id') == rule_id]
        _require(len(matching) <= 1)
        rule = matching[0] if matching else None
    locations = result.get('locations')
    _require(isinstance(locations, list) and locations and isinstance(locations[0], dict))
    physical = locations[0].get('physicalLocation')
    _require(isinstance(physical, dict))
    artifact = physical.get('artifactLocation')
    region = physical.get('region')
    _require(isinstance(artifact, dict) and isinstance(region, dict))
    # SARIF permits character coordinates without a line number. Validate
    # every supplied coordinate, without opening the referenced source to
    # derive a line or exposing the region's snippet.
    if 'startLine' in region:
        _require(type(region['startLine']) is int and region['startLine'] > 0)
    for coordinate in ('charOffset', 'charLength'):
        if coordinate in region:
            _require(type(region[coordinate]) is int and region[coordinate] >= 0)
    _require('startLine' in region or 'charOffset' in region)
    row = {'rule_id': rule_id, 'path': _path(artifact.get('uri'))}
    if 'startLine' in region:
        row['line'] = region['startLine']
    level = result.get('level')
    if isinstance(level, str) and level in {'none', 'note', 'warning', 'error'}:
        row['level'] = level
    properties = rule.get('properties', {}) if rule is not None else {}
    if isinstance(properties, dict):
        severity = properties.get('security-severity')
        if isinstance(severity, (str, int, float)) and not isinstance(severity, bool):
            try:
                numeric = float(severity)
                if math.isfinite(numeric) and 0 <= numeric <= 10:
                    row['security_severity'] = numeric
            except (ValueError, OverflowError):
                pass
    return row


def load_results(directory: Path) -> list[dict]:
    try:
        _require(not directory.is_symlink() and directory.is_dir())
        # Reject symlinked ancestors as well as the immediate input directory.
        _require(not any(parent.is_symlink() for parent in directory.parents))
        files = []
        for path in directory.iterdir():
            if path.suffix == '.sarif':
                files.append(path)
                _require(len(files) <= MAX_FILES)
        _require(files)
        rows = []
        for path in sorted(files):
            _require(not path.is_symlink() and path.is_file() and path.stat().st_size <= MAX_BYTES)
            with path.open('rb') as source:
                data = source.read(MAX_BYTES + 1)
            _require(len(data) <= MAX_BYTES)
            payload = json.loads(data)
            _require(isinstance(payload, dict) and payload.get('version') == '2.1.0')
            runs = payload.get('runs')
            _require(isinstance(runs, list))
            for run in runs:
                _require(isinstance(run, dict) and isinstance(run.get('tool'), dict))
                driver = run['tool'].get('driver')
                _require(isinstance(driver, dict))
                rules = driver.get('rules', [])
                results = run.get('results')
                _require(isinstance(rules, list) and isinstance(results, list))
                _require(len(rows) + len(results) <= MAX_RESULTS)
                rows.extend(_row(result, rules) for result in results)
        return rows
    except (OSError, UnicodeError, ValueError, TypeError, RecursionError) as error:
        raise ValueError('Invalid CodeQL metadata') from error


def main():
    try:
        _require(len(sys.argv) == 2)
        rows = load_results(Path(sys.argv[1]))
    except ValueError:
        print('CodeQL metadata unavailable or invalid', file=sys.stderr)
        return 1
    for row in rows:
        print(json.dumps(row, sort_keys=True))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
