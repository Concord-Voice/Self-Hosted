#!/usr/bin/env python3
"""Resolve this checkout's exact tooling sources in a user-owned cache."""
import argparse
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile

PUBLIC = "Agentic-Dev-Tooling"
PRIVATE = "Internal-Dev-Tooling"
PLATFORMS = ("claude", "codex", "copilot", "antigravity")
FLEET_WORKFLOWS = dict.fromkeys((
    "ci-status", "dev-lifecycle", "enhanced-pr-review", "issue-brief", "issue-creation",
    "pr-creation", "reproduce-then-fix", "review-pr", "verify-quality-gate",
), "core-lifecycle")
FLEET_WORKFLOWS["weekly-deps"] = "release-ops"
SKILL_FILE = "SKILL.md"
DEPLOYED = Path(__file__).absolute()
PROJECT = DEPLOYED.parent.parent


class ToolingError(Exception):
    pass


def inside(path, root):
    return path == root or root in path.parents


def git_env():
    # Git may export repository-local variables from hooks or worktrees.
    env = {k: v for k, v in os.environ.items() if k not in {
        "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
        "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
        "GIT_CEILING_DIRECTORIES", "GIT_PREFIX", "GIT_NAMESPACE",
    }}
    env.update(GIT_TERMINAL_PROMPT="0", GCM_INTERACTIVE="Never",
               GIT_ASKPASS="false", SSH_ASKPASS="false")
    return env


def git(root, *args):
    env = git_env()
    if args[0] == "fetch":
        # Git may rewrite the HTTPS source to SSH through a configured insteadOf rule.
        ssh = env.get("GIT_SSH_COMMAND")
        if not ssh:
            config = subprocess.run(["git", "-C", str(root), "config", "--get", "core.sshCommand"],
                                    capture_output=True, text=True, env=env, check=False)
            ssh = config.stdout.strip() or shlex.quote(env.get("GIT_SSH", "ssh"))
        env["GIT_SSH_COMMAND"] = ssh + " -o BatchMode=yes"
    return subprocess.run(
        ["git", "--no-replace-objects", "-c", "core.hooksPath=" + os.devnull, "-c", "init.templateDir=",
         "-C", str(root), *args], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, env=env, check=True, text=True,
    ).stdout.strip()


def verify_cache_directory(path, info):
    if stat.S_ISLNK(info.st_mode):
        raise ToolingError("symlink refused in cache location; set XDG_CACHE_HOME to its physical absolute path")
    if not stat.S_ISDIR(info.st_mode):
        raise ToolingError(f"cache ancestor is not a directory: {path}")
    sticky_root = info.st_uid == 0 and bool(info.st_mode & stat.S_ISVTX)
    if info.st_uid not in (0, os.getuid()) or (info.st_mode & 0o022 and not sticky_root):
        raise ToolingError(f"untrusted cache ancestor: {path}")


def validate_cache_parents(cache, *, create=False):
    # Walk from the root. A verified ancestor cannot be replaced by another user.
    # A root-owned sticky temp directory also protects its users' existing entries.
    for path in (*reversed(cache.parents), cache):
        try:
            info = path.lstat()
        except FileNotFoundError:
            if not create:
                return path.parent
            path.mkdir(mode=0o700, exist_ok=True)
            info = path.lstat()
        verify_cache_directory(path, info)
    return cache


def cache_root():
    if os.name != "posix" or not callable(getattr(os, "getuid", None)):
        raise ToolingError("Windows checkouts require WSL. Run this command in WSL with a cache in your Linux home directory.")
    base = Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache")
    if not base.is_absolute() or ".." in base.parts:
        raise ToolingError("XDG_CACHE_HOME must be a normalized absolute path")
    cache = base / "concord-voice" / "tooling"
    ancestor = validate_cache_parents(cache)
    if inside(cache.resolve(), PROJECT.resolve()):
        raise ToolingError("tooling cache must be outside the checkout")
    if shutil.which("git") is None:
        raise ToolingError("Git is required to verify tooling checkouts")
    try:
        git(ancestor, "rev-parse", "--git-dir")
        raise ToolingError("set XDG_CACHE_HOME to a private directory outside Git worktrees and Git directories")
    except subprocess.CalledProcessError:
        pass
    validate_cache_parents(cache, create=True)
    for path in (cache.parent, cache):
        info = path.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ToolingError("tooling cache must be private to its owner (mode 0700)")
    return cache


def verified_checkout(directory, revision):
    if directory.is_symlink() or (directory / ".git").is_symlink() or not (directory / ".git").is_dir():
        raise ToolingError(f"invalid tooling cache entry at {directory}; choose a fresh private cache directory")
    try:
        head = git(directory, "rev-parse", "--verify", "HEAD")
        dirty = git(directory, "status", "--porcelain", "--untracked-files=all", "--ignored")
    except subprocess.CalledProcessError:
        raise ToolingError(f"cannot verify cached tooling checkout at {directory}; choose a fresh private cache directory") from None
    if head != revision or dirty:
        raise ToolingError(f"cached tooling checkout changed at {directory}; choose a fresh private cache directory")
    return directory


class SourceUnavailable(ToolingError):
    pass


def acquire(cache, repo, revision, offline):
    bucket = cache / repo
    if bucket.is_symlink():
        raise ToolingError("symlink refused in cache location")
    bucket.mkdir(exist_ok=True, mode=0o700)
    target = bucket / revision
    if target.exists() or target.is_symlink():
        return verified_checkout(target, revision)
    if offline:
        raise SourceUnavailable("pinned revision is absent from the offline cache")
    staging = Path(tempfile.mkdtemp(prefix="fetch-", dir=bucket))
    try:
        git(staging, "init", "--quiet")
        try:
            git(staging, "fetch", "--quiet", "--no-tags", "--depth=1",
                "https://github.com/Concord-Voice/" + repo + ".git", revision)
        except subprocess.CalledProcessError:
            raise SourceUnavailable("pinned revision unavailable with current Git access") from None
        # Keep cached scripts byte-identical to the reviewed commit on Windows as well.
        git(staging, "config", "--local", "core.autocrlf", "false")
        git(staging, "checkout", "--quiet", "--detach", "FETCH_HEAD")
        verified_checkout(staging, revision)
        try:
            staging.rename(target)
        except OSError:
            if not target.is_dir():
                raise
            # A concurrent bootstrap may already have published this exact revision.
        return verified_checkout(target, revision)
    finally:
        if staging.exists():
            shutil.rmtree(staging)


def skill_file(root, path):
    if not inside(path.resolve(), root) or any(p.is_symlink() for p in (path, *path.parents) if inside(p, root)):
        raise ToolingError("skill resolves outside its pinned source or uses a symlink")
    return path if path.is_file() else None


def fleet_workflow(public, name):
    """Resolve the reviewed fleet edition from its public owner and verify its return path."""
    helper = skill_file(public, public / "scripts/resolve-fleet-workflow.py")
    if helper is None:
        raise ToolingError("pinned public owner lacks the defensive fleet workflow resolver")
    cache = cache_root()
    env = {key: os.environ[key] for key in ("PATH", "HOME", "SystemRoot", "TMPDIR") if key in os.environ}
    result = subprocess.run(
        [sys.executable, "-I", "-B", str(helper), "--cache", str(cache), "--skill", name],
        cwd=public, env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True,
        check=False,
    )
    lines = result.stdout.splitlines()
    if result.returncode or len(lines) != 1:
        raise ToolingError("defensive fleet workflow unavailable: " + name)
    path = Path(lines[0])
    bucket = cache / "fleet-workflows"
    if not path.is_absolute() or path.resolve() != path or not inside(path, bucket):
        raise ToolingError("invalid defensive fleet workflow path")
    parts = path.relative_to(bucket).parts
    if (len(parts) != 4 or not re.fullmatch("[0-9a-f]{64}", parts[0])
            or parts[1:] != ("skills", name, SKILL_FILE) or not path.is_file()):
        raise ToolingError("invalid defensive fleet workflow entry")
    return path


def reviewed_superpowers(source):
    helper = skill_file(source, source / "scripts/project-superpowers.py")
    if helper is None:
        raise ToolingError("private pin lacks the reviewed Superpowers projection")
    cache = cache_root()
    pack = Path(subprocess.check_output(
        [sys.executable, "-I", "-B", str(helper), "--cache", str(cache)], text=True
    ).strip())
    if not inside(pack, cache) or pack.resolve() != pack:
        raise ToolingError("reviewed Superpowers projection escaped its cache")
    return pack


def declared_plugin_skill(source, manifest_dir, namespace, name):
    pack = source / "plugins" / namespace
    manifest = skill_file(source, pack / manifest_dir / "plugin.json")
    if manifest is None:
        return None
    declared = json.loads(manifest.read_bytes())
    if (not isinstance(declared, dict) or declared.get("name") != namespace
            or declared.get("skills", "./skills/") not in ("skills", "./skills", "./skills/")):
        raise ToolingError("unrecognized plugin skill declaration: " + namespace)
    entry = Path("skills") / name / SKILL_FILE
    found = skill_file(source, pack / entry)
    if found and namespace == "superpowers":
        # Only expose the reviewed projection after checking its upstream entry.
        pack = reviewed_superpowers(source)
        found = skill_file(pack, pack / entry)
    if found is None:
        raise ToolingError("skill absent from pinned plugin: " + namespace + ":" + name)
    return found


def namespaced_skill(namespace, name, public, private, private_status):
    # Read only declared packs at exact pinned paths. A plugin installed through
    # the active host has its own catalog and trust boundaries.
    sources = ((public, ".claude-plugin"), (private, ".codex-plugin"))
    if namespace == "superpowers":
        sources = ((private, ".codex-plugin"),)
    if namespace == "microsoft-foundry":
        raise ToolingError("standalone Foundry projection unavailable; use an approved hardened plugin "
                           "in the active client's catalog")
    for source, manifest_dir in sources:
        if source is None:
            continue
        found = declared_plugin_skill(source, manifest_dir, namespace, name)
        if found:
            return found
    suffix = " (private source unavailable: " + private_status + ")" if private is None else ""
    raise ToolingError("plugin absent from pinned catalog: " + namespace
                       + "; resolve an approved installed plugin in the active client's catalog" + suffix)


def private_feedback_skill(namespace, private):
    name = "feedback-triage"
    if namespace not in (None, "release-ops"):
        raise ToolingError("namespace does not own the Concord feedback policy")
    if private is None:
        raise ToolingError("private-unavailable: " + name + " requires the pinned Concord owner")
    found = skill_file(private, private / "skills" / name / SKILL_FILE)
    if not found:
        raise ToolingError("required private skill is missing at its pin: " + name)
    return found


def resolve_skill(name, public, private, private_status):
    pieces = name.split(":")
    if len(pieces) > 2 or any(not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,100}", p) for p in pieces):
        raise ToolingError("invalid skill name")
    name = pieces[-1]
    namespace = pieces[0] if len(pieces) == 2 else None
    fleet_name = name.removeprefix("fleet-")
    if fleet_name in FLEET_WORKFLOWS:
        if namespace is not None and namespace != FLEET_WORKFLOWS[fleet_name]:
            raise ToolingError("namespace does not own this defensive fleet workflow")
        return fleet_workflow(public, fleet_name)
    if name.startswith("fleet-"):
        raise ToolingError("unknown defensive fleet workflow: " + name)
    if name in ("concord-feedback-triage", "feedback-triage"):
        return private_feedback_skill(namespace, private)
    if namespace is not None:
        return namespaced_skill(namespace, name, public, private, private_status)
    return unqualified_skill(name, public, private, private_status)


def unqualified_skill(name, public, private, private_status):
    matches = [skill_file(public, p) for p in sorted((public / "plugins").glob(f"*/skills/{name}/{SKILL_FILE}"))]
    matches = [p for p in matches if p]
    if len(matches) == 1:
        return matches[0]
    if len(matches) > 1:
        raise ToolingError("multiple public owners for skill: " + name)
    if private is not None:
        found = skill_file(private, private / "skills" / name / SKILL_FILE)
        if found:
            return found
    suffix = "" if private is not None else " (private-unavailable: " + private_status + ")"
    raise ToolingError("skill absent from available pinned sources: " + name + suffix)


def install_public(platform, public):
    # Only a canonical name from this fixed tuple reaches the installer.
    native_platform = next((known for known in PLATFORMS if known == platform), None)
    if native_platform is None:
        raise ToolingError("unsupported public installer platform")
    installer = public / "install.sh"
    if installer.is_symlink() or not installer.is_file():
        raise ToolingError("pinned public native installer is missing")
    expected_blob = git(public, "rev-parse", "HEAD:install.sh")
    if git(public, "hash-object", "--no-filters", "--", "install.sh") != expected_blob:
        raise ToolingError("public installer differs from the pinned Git blob; select a fresh private XDG_CACHE_HOME")
    # Older pins have no narrow profile mode. Never hand them an unknown flag.
    if "--agents-only" not in installer.read_text(encoding="utf-8"):
        raise ToolingError("public pin lacks --agents-only support; update to a reviewed native installer")
    subprocess.run(["bash", "--", str(installer), native_platform, "--agents-only", "--project", str(PROJECT)],
                   cwd=PROJECT, env={k: os.environ[k] for k in ("PATH", "HOME", "TMPDIR", "SystemRoot")
                                     if k in os.environ}, stdin=subprocess.DEVNULL, check=True)


CAPABILITY_CHECKS = {
    "agent": "verify-native-role-and-permissions",
    "knowledge": "readable", "rule": "readable",
    "hook": "verify-host-registration-and-trust",
    "settings": "verify-host-configuration",
    "mcp": "verify-server-trust-and-tool-availability",
    "plugin": "verify-plugin-installation-and-enablement",
    "marketplace": "verify-host-marketplace-registration",
    "script": "verify-caller-workflow-and-prerequisites",
}


def catalog_item(private, key, relative):
    if not isinstance(key, str) or not re.fullmatch(r"[a-z]+:[a-z0-9][a-z0-9-]*", key):
        raise ToolingError("invalid capability key; use kind:name")
    kind = key.split(":", 1)[0]
    if kind not in CAPABILITY_CHECKS or not isinstance(relative, str):
        raise ToolingError("capability absent from the pinned private catalog")
    if set(relative.split("/")) & {"", ".", "..", ".git"} or "\\" in relative or Path(relative).is_absolute():
        raise ToolingError("invalid path in private capability catalog")
    asset = skill_file(private, private / relative)
    if asset is None:
        raise ToolingError("missing private catalog file for " + key)
    return {"path": str(asset), "source_path": relative, "runtime_check": CAPABILITY_CHECKS[kind]}


def private_catalog(private, revision, selector):
    if private is None:
        raise ToolingError("private-unavailable: catalog requires the pinned private source")
    manifest = skill_file(private, private / "knowledge/capabilities.json")
    if manifest is None:
        raise ToolingError("private pin has no capability catalog")
    entries = json.loads(manifest.read_bytes())
    if not isinstance(entries, dict):
        raise ToolingError("invalid private capability catalog")
    # Only the reviewed manifest inside the authenticated source supplies keys
    # and paths. Listing a role or script never executes or installs it.
    selected = entries if selector == "*" else {selector: entries.get(selector)}
    result = {key: catalog_item(private, key, relative) for key, relative in sorted(selected.items())}
    return {"source": PRIVATE, "revision": revision, "capabilities": result}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--offline", action="store_true", help="require a populated local cache")
    parser.add_argument("--public-only", action="store_true", help="skip the authenticated private source")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--skill", metavar="NAME", help="print the canonical SKILL.md path")
    mode.add_argument("--private-catalog", nargs="?", const="*", metavar="KIND:NAME",
                      help="list approved private capability sources or inspect one (no activation)")
    mode.add_argument("--install-public", choices=PLATFORMS, help="install public native workflow profiles")
    args = parser.parse_args()
    lock = DEPLOYED.parent / "sources.json"
    if DEPLOYED.is_symlink() or DEPLOYED.parent.is_symlink() or lock.is_symlink():
        raise ToolingError("symlink refused in consumer tooling path")
    pins = json.loads(lock.read_text())
    if not isinstance(pins, dict) or set(pins) != {"public", "private"} or not all(
        isinstance(v, str) and re.fullmatch(r"[0-9a-f]{40}", v) for v in pins.values()
    ):
        raise ToolingError("invalid tooling lock; expected two full Git commit IDs")
    cache = cache_root()
    public = acquire(cache, PUBLIC, pins["public"], args.offline)
    private = None
    private_status = "public-only"
    if not args.public_only and not args.install_public:
        try:
            private = acquire(cache, PRIVATE, pins["private"], args.offline)
            private_status = "available"
        except SourceUnavailable as error:
            private_status = "private-unavailable: " + str(error)
            print(private_status, file=sys.stderr)
    if args.private_catalog is not None:
        print(json.dumps(private_catalog(private, pins["private"], args.private_catalog), indent=2))
    elif args.skill:
        print(resolve_skill(args.skill, public, private, private_status))
    elif args.install_public:
        install_public(args.install_public, public)
    else:
        print(json.dumps({"public": str(public), "private": str(private) if private else None,
                          "private_status": private_status}))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.CalledProcessError, ToolingError) as error:
        sys.exit("Concord tooling: " + str(error))
