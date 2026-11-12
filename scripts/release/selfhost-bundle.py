#!/usr/bin/env python3
"""Deterministic self-host bundle assets built from one tracked source tree.

The manifest names the version and source commit; neither authorizes publication.
"""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tarfile


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def safe_path(name):
    require(isinstance(name, str) and name and not name.startswith('/') and
            '\\' not in name and all(p not in ('', '.', '..') for p in name.split('/'))
            and not any(ord(c) < 32 or ord(c) == 127 for c in name), 'unsafe payload path')


def identities(version, source):
    require(isinstance(version, str) and re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', version),
            'invalid canonical version')
    require(isinstance(source, str) and re.fullmatch('[0-9a-f]{40}', source), 'invalid full SHA identity')


def inventory(tree):
    require(tree.is_dir() and not tree.is_symlink(), 'public-tree must be a directory')
    files = {}
    for base, dirs, names in os.walk(tree, followlinks=False):
        # Git metadata is transport, never release payload.
        if Path(base) == tree:
            dirs[:] = [d for d in dirs if d != '.git']
            names = [n for n in names if n != '.git']
        for name in dirs + names:
            path = Path(base) / name
            relative = path.relative_to(tree).as_posix()
            safe_path(relative)
            mode = path.lstat().st_mode
            require(stat.S_ISDIR(mode) or stat.S_ISREG(mode), 'unsafe payload type/link')
            if stat.S_ISREG(mode):
                require(relative != 'selfhost-manifest.json', 'payload contains reserved manifest')
                files[relative] = (path.read_bytes(), 0o755 if mode & 0o111 else 0o644)
    require(files, 'empty public-tree payload')
    return dict(sorted(files.items()))


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, 'duplicate manifest key')
        result[key] = value
    return result


def build(args):
    identities(args.version, args.source_sha)
    epoch = args.source_epoch
    if epoch is None:
        repo = Path(__file__).resolve().parents[2]
        epoch = int(subprocess.check_output(['git', '-C', str(repo), 'show', '-s', '--format=%ct', args.source_sha], text=True))
    require(epoch >= 0, 'invalid source epoch')
    files = inventory(args.public_tree)
    root = f'concord-selfhost-{args.version}'
    manifest = dict(schema='selfhost-bundle/v2', tag=f'v{args.version}',
                    version=args.version, source_sha=args.source_sha,
                    root=root, profile='services', files={p: digest(v[0]) for p, v in files.items()})
    encoded = (json.dumps(manifest, sort_keys=True, indent=2) + '\n').encode()
    require(not args.out.is_symlink() and (not args.out.exists() or args.out.is_dir()), 'output must be a directory')
    require(args.out.resolve() != args.public_tree.resolve() and
            args.public_tree.resolve() not in args.out.resolve().parents, 'output cannot be inside payload')
    args.out.mkdir(parents=True, exist_ok=True)
    archive = args.out / f'{root}.tar.gz'
    for name in (archive.name, 'selfhost-manifest.json', 'SHA256SUMS'):
        require(not (args.out / name).is_symlink(), 'unsafe output link')
    payload = dict(files)
    payload['selfhost-manifest.json'] = (encoded, 0o644)
    with archive.open('wb') as raw:
        with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as tar:
                for name, (data, mode) in sorted(payload.items()):
                    info = tarfile.TarInfo(f'{root}/{name}')
                    info.size, info.mode, info.mtime = len(data), mode, epoch
                    info.uid = info.gid = 0
                    info.uname = info.gname = ''
                    tar.addfile(info, io.BytesIO(data))
    (args.out / 'selfhost-manifest.json').write_bytes(encoded)
    assets = (archive, args.out / 'selfhost-manifest.json')
    (args.out / 'SHA256SUMS').write_text(''.join(f'{digest(p.read_bytes())}  {p.name}\n' for p in sorted(assets)))


def verify(args):
    encoded = args.manifest.read_bytes()
    manifest = json.loads(encoded, object_pairs_hook=unique_object)
    require(isinstance(manifest, dict) and set(manifest) ==
            {'schema', 'tag', 'version', 'source_sha', 'root', 'profile', 'files'}, 'invalid manifest schema fields')
    require(manifest['schema'] == 'selfhost-bundle/v2', 'invalid manifest schema')
    identities(manifest['version'], manifest['source_sha'])
    root = f"concord-selfhost-{manifest['version']}"
    require(manifest['root'] == root and manifest['tag'] == f"v{manifest['version']}"
            and manifest['profile'] == 'services', 'invalid manifest identity/profile')
    require(args.archive.name == f'{root}.tar.gz' and args.manifest.name == 'selfhost-manifest.json'
            and args.sums.name == 'SHA256SUMS', 'invalid checksum asset names')
    expected_sums = ''.join(f'{digest(p.read_bytes())}  {p.name}\n' for p in sorted((args.archive, args.manifest)))
    require(args.sums.read_bytes() == expected_sums.encode(), 'SHA256 checksum mismatch')
    files = inventory(args.public_tree)
    recorded = manifest['files']
    require(isinstance(recorded, dict) and list(recorded) == sorted(recorded), 'invalid manifest inventory order')
    for name, checksum in recorded.items():
        safe_path(name)
        require(name != 'selfhost-manifest.json', 'manifest inventories itself')
        require(isinstance(checksum, str) and re.fullmatch('[0-9a-f]{64}', checksum), 'invalid payload digest')
    require(recorded == {p: digest(v[0]) for p, v in files.items()}, 'public-tree payload snapshot mismatch')
    expected = dict(files)
    expected['selfhost-manifest.json'] = (encoded, 0o644)
    seen = set()
    epoch = None
    previous = ''
    # Verification never extracts or writes. Every member is checked against trusted bytes.
    with tarfile.open(args.archive, 'r:gz') as tar:
        for member in tar:
            safe_path(member.name)
            require(member.name not in seen, 'duplicate archive path')
            require(member.name > previous, 'archive path order drift')
            previous = member.name
            seen.add(member.name)
            require(member.isfile() and not member.linkname, 'unsafe archive type/link/device')
            require(not member.pax_headers, 'unexpected archive metadata type')
            require(member.name.startswith(root + '/'), 'archive path outside root')
            relative = member.name[len(root) + 1:]
            require(relative in expected, 'unexpected archive payload path')
            data, mode = expected[relative]
            require(member.mode == mode, 'archive mode/permission drift')
            require((member.uid, member.gid, member.uname, member.gname) == (0, 0, '', ''), 'archive ownership drift')
            if epoch is None:
                epoch = member.mtime
            require(member.mtime == epoch and isinstance(epoch, int) and epoch >= 0, 'archive timestamp drift')
            require(member.size == len(data), 'archive payload/manifest size mismatch')
            require(tar.extractfile(member).read() == data,
                    'archive manifest mismatch' if relative == 'selfhost-manifest.json' else 'archive payload digest mismatch')
    require(seen == {root + '/' + p for p in expected}, 'missing archive payload')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    builder = sub.add_parser('build')
    for flag in ('source-sha', 'version'):
        builder.add_argument('--' + flag, required=True)
    builder.add_argument('--source-epoch', type=int)
    builder.add_argument('--out', type=Path, required=True)
    checker = sub.add_parser('verify')
    for flag in ('archive', 'manifest', 'sums'):
        checker.add_argument('--' + flag, type=Path, required=True)
    for command in (builder, checker):
        command.add_argument('--public-tree', type=Path, required=True)
    args = parser.parse_args()
    try:
        (build if args.command == 'build' else verify)(args)
    except (ValueError, TypeError, EOFError, OSError, tarfile.TarError, subprocess.SubprocessError) as error:
        print(f'selfhost bundle refused: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
