#!/usr/bin/env python3
"""Shared fixtures and validator/workflow cases for self-host acceptance (#2505).

These fixtures are owned projections of the selected Compose model and Docker
inspection data. They deliberately contain no container Env, command output,
credentials, or host data.
"""
from __future__ import annotations

import importlib.util
import hashlib
import io
import copy
from collections import Counter
import ipaddress
import json
import math
import os
import signal
import shutil
import subprocess
import time
from types import SimpleNamespace
from pathlib import Path
import re
import sys
import tempfile
import tarfile
import unittest
from unittest import mock


TEST_BASH = shutil.which("bash")
if TEST_BASH is None:
    raise RuntimeError("self-host acceptance tests require Bash")
TEST_PATH = str(Path(TEST_BASH).parent) + os.pathsep + os.defpath

ROOT = Path(__file__).resolve().parents[3]
HELPER = ROOT / "infrastructure/deploy/tests/selfhost-acceptance.py"
SPEC = importlib.util.spec_from_file_location("selfhost_acceptance", HELPER)
if SPEC is None or SPEC.loader is None:
    raise ImportError("self-host acceptance helper is unavailable")
acceptance = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = acceptance
SPEC.loader.exec_module(acceptance)

PROJECT = "acceptance-owned"
SERVICES = ("postgres", "redis", "nats", "coturn", "minio", "ops-agent",
            "control-plane", "media-plane")
PUBLIC_SERVICES = tuple(service for service in SERVICES if service != "ops-agent")
CONTAINER_NAMES = {service: f"concordvoice-{service}" for service in SERVICES}


def binding(target: int, published: int | None = None, protocol: str = "tcp",
            host_ip: str = "") -> dict:
    value = {"target": target, "protocol": protocol}
    if published is not None:
        value["published"] = published
    if host_ip:
        value["host_ip"] = host_ip
    return value


def port_range(start: int, end: int, protocol: str) -> dict:
    return {"target": f"{start}-{end}", "published": f"{start}-{end}",
            "protocol": protocol}


def rendered_model(project: str = PROJECT) -> dict:
    """Effective three-file Compose projection with services profile selected."""
    model = {
        "name": project,
        "services": {
            "postgres": {"container_name": CONTAINER_NAMES["postgres"],
                         "ports": [], "volumes": []},
            "redis": {"container_name": CONTAINER_NAMES["redis"],
                      "ports": [], "volumes": []},
            "nats": {"container_name": CONTAINER_NAMES["nats"],
                     "ports": [], "volumes": []},
            "coturn": {
                "container_name": CONTAINER_NAMES["coturn"],
                "ports": [binding(3478, 3478, "tcp"),
                          binding(3478, 3478, "udp"),
                          binding(5349, 5349, "tcp"),
                          port_range(49152, 49252, "udp")],
                "volumes": [
                    {"type": "bind",
                     "source": "/workspace/checkout/infrastructure/docker/coturn/turnserver.conf",
                     "target": "/etc/coturn/turnserver.conf", "read_only": True},
                    {"type": "bind", "source": "/opt/concord/certs/coturn",
                     "target": "/etc/coturn/certs", "read_only": True},
                ],
            },
            "minio": {"container_name": CONTAINER_NAMES["minio"],
                      "ports": [], "volumes": []},
            "ops-agent": {"container_name": CONTAINER_NAMES["ops-agent"],
                          "ports": [], "volumes": []},
            "control-plane": {"container_name": CONTAINER_NAMES["control-plane"],
                              "ports": [binding(8080, 8080, "tcp", "127.0.0.1")],
                              "volumes": []},
            "media-plane": {
                "container_name": CONTAINER_NAMES["media-plane"],
                "ports": [binding(3000, 3000, "tcp", "127.0.0.1"),
                          port_range(40000, 41999, "udp")],
                "volumes": [],
            },
        },
    }
    quotas = {"postgres": "1.5", "redis": "1", "nats": "1", "coturn": "1",
              "minio": "1", "ops-agent": "0.25", "control-plane": "1.5", "media-plane": "2"}
    for service, quota in quotas.items():
        model["services"][service]["deploy"] = {"resources": {"limits": {"cpus": quota}}}
    model["services"]["media-plane"]["environment"] = {"NUM_WORKERS": "2"}
    return model


def live_projection(model: dict | None = None) -> list[dict]:
    model = model or rendered_model()
    images = {"postgres": "concordvoice-postgres:16-alpine",
              "redis": "redis:8-alpine", "nats": "nats:2-alpine",
              "coturn": "coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e",
              "minio": "ghcr.io/concord-voice/minio@sha256:fixture",
              "ops-agent": "concordvoice-ops-agent:local",
              "control-plane": "concordvoice-control-plane:local",
              "media-plane": "concordvoice-media-plane:local"}
    result = []
    for service in model["services"]:
        cfg = model["services"][service]
        state = "exited" if service == "ops-agent" else "running"
        observed = {"id": f"owned-{service}-id", "name": f"/{cfg['container_name']}",
                    "service": service, "project": model["name"], "image": images[service],
                    "state": state,
                    "health": "none" if service in ("minio", "ops-agent") else "healthy",
                    "exit_code": 0 if service == "ops-agent" else None,
                    "ports": [],
                    "mounts": [dict(m) for m in cfg["volumes"]]}
        # Engine inspection reports concrete ports and addresses. A wildcard
        # Compose mapping is observed as IPv4, with IPv6 recorded independently.
        for port in cfg["ports"]:
            start, _, end = str(port["target"]).partition("-")
            target_end = int(end) if end else int(start)
            for number in range(int(start), target_end + 1):
                published_start, _, published_end = str(port["published"]).partition("-")
                published = int(published_start) + number - int(start)
                host_ip = port.get("host_ip") or "0.0.0.0"
                observed["ports"].append({"target": number, "published": published,
                                           "protocol": port["protocol"], "host_ip": host_ip})
                if not port.get("host_ip"):
                    observed["ports"].append({"target": number, "published": published,
                                               "protocol": port["protocol"], "host_ip": "::"})
        result.append(observed)
    return result


def public_rendered_model(project: str = PROJECT) -> dict:
    """Public producer selects seven services; the managed agent is absent."""
    model = rendered_model(project)
    del model["services"]["ops-agent"]
    return model




class SelfhostAcceptanceTests(unittest.TestCase):
    def assert_code(self, code: str, call, *args, **kwargs) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            call(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def test_complete_selected_model(self):
        model = rendered_model()
        got = acceptance.validate_rendered(model, PROJECT)
        expected = {
            acceptance.Binding("control-plane", "127.0.0.1", 8080, 8080, "tcp"),
            acceptance.Binding("media-plane", "127.0.0.1", 3000, 3000, "tcp"),
        }
        expected.update(acceptance.Binding("coturn", "0.0.0.0", port, port, protocol)
                        for port, protocol in ((3478, "tcp"), (3478, "udp"), (5349, "tcp")))
        expected.update(acceptance.Binding("media-plane", "0.0.0.0", port, port, "udp")
                        for port in range(40000, 42000))
        expected.update(acceptance.Binding("coturn", "0.0.0.0", port, port, "udp")
                        for port in range(49152, 49253))
        self.assertEqual(got, expected)
        verdict = acceptance.validate_live(live_projection(model), PROJECT, model)
        self.assertEqual(verdict["status"], "PASS")
        live_bindings = verdict["bindings"]
        ipv4 = {(item["service"], item["target"], item["published"], item["protocol"],
                 item["host_ip"])
                for item in live_bindings if item["host_ip"] in ("0.0.0.0", "127.0.0.1")}
        ipv6 = {(item["service"], item["target"], item["published"], item["protocol"])
                for item in live_bindings if item["host_ip"] == "::"}
        expected_ranges = {
            (service, port, port, "udp")
            for service, first, last in (("media-plane", 40000, 41999),
                                         ("coturn", 49152, 49252))
            for port in range(first, last + 1)
        }
        expected_ipv4 = {
            (service, target, published, protocol, "0.0.0.0")
            for service, target, published, protocol in expected_ranges
        } | {
            ("control-plane", 8080, 8080, "tcp", "127.0.0.1"),
            ("media-plane", 3000, 3000, "tcp", "127.0.0.1"),
            ("coturn", 3478, 3478, "tcp", "0.0.0.0"),
            ("coturn", 3478, 3478, "udp", "0.0.0.0"),
            ("coturn", 5349, 5349, "tcp", "0.0.0.0"),
        }
        self.assertEqual(ipv4, expected_ipv4)
        expected_ipv6 = expected_ranges | {
            ("coturn", 3478, 3478, "tcp"), ("coturn", 3478, 3478, "udp"),
            ("coturn", 5349, 5349, "tcp"),
        }
        self.assertEqual(ipv6, expected_ipv6)

    def test_public_bundle_seven_service_model_and_live_observations(self):
        model = public_rendered_model()
        expected = acceptance.validate_rendered(rendered_model(), PROJECT)
        self.assertEqual(acceptance.validate_rendered(model, PROJECT, public_bundle=True), expected)
        verdict = acceptance.validate_live(live_projection(model), PROJECT, model, public_bundle=True)
        self.assertEqual(verdict["status"], "PASS")
        self.assertNotIn("ops-agent", {row["service"] for row in live_projection(model)})

    def test_selected_service_contract_cannot_be_inferred_from_missing_agent(self):
        self.assert_code("RENDERED_SERVICE_SET_INVALID", acceptance.validate_rendered,
                         public_rendered_model(), PROJECT)
        self.assert_code("RENDERED_SERVICE_SET_INVALID", acceptance.validate_rendered,
                         rendered_model(), PROJECT, public_bundle=True)
        for service in PUBLIC_SERVICES:
            model = public_rendered_model()
            del model["services"][service]
            with self.subTest(missing=service):
                self.assert_code("RENDERED_SERVICE_SET_INVALID", acceptance.validate_rendered,
                                 model, PROJECT, public_bundle=True)
        model = public_rendered_model()
        model["services"]["foreign-service"] = {"ports": [], "volumes": []}
        self.assert_code("RENDERED_SERVICE_SET_INVALID", acceptance.validate_rendered,
                         model, PROJECT, public_bundle=True)

    def test_public_bundle_live_service_set_refuses_missing_extra_and_duplicates(self):
        model = public_rendered_model()
        for service in PUBLIC_SERVICES:
            rows = [row for row in live_projection(model) if row["service"] != service]
            with self.subTest(missing=service):
                self.assert_code("LIVE_SERVICE_SET_INVALID", acceptance.validate_live,
                                 rows, PROJECT, model, public_bundle=True)
        for service in ("ops-agent", "foreign-service"):
            rows = live_projection(model)
            extra = copy.deepcopy(live_projection()[0])
            extra.update(id="owned-extra-id", service=service, name="/concordvoice-" + service)
            rows.append(extra)
            with self.subTest(extra=service):
                self.assert_code("LIVE_SERVICE_SET_INVALID", acceptance.validate_live,
                                 rows, PROJECT, model, public_bundle=True)
        rows = live_projection(model)
        rows.append(dict(rows[0], id="owned-duplicate-id"))
        self.assert_code("LIVE_IDENTITY_INVALID", acceptance.validate_live,
                         rows, PROJECT, model, public_bundle=True)

    def test_public_bundle_cannot_select_managed_agent_via_profile(self):
        model = public_rendered_model()
        model["services"]["ops-agent"] = dict(rendered_model()["services"]["ops-agent"],
                                             profiles=["services"])
        self.assert_code("RENDERED_SERVICE_SET_INVALID", acceptance.validate_rendered,
                         model, PROJECT, public_bundle=True)
        model["services"]["ops-agent"]["profiles"] = False
        self.assert_code("RENDERED_SERVICE_INVALID", acceptance.validate_rendered,
                         model, PROJECT, public_bundle=True)

    def test_missing_required_port(self):
        model = rendered_model()
        model["services"]["coturn"]["ports"].pop()
        self.assert_code("RENDERED_PORT_MISSING", acceptance.validate_rendered, model, PROJECT)
        model = rendered_model()
        observations = live_projection(model)
        turn = next(item for item in observations if item["service"] == "coturn")
        turn["ports"] = [item for item in turn["ports"]
                         if not (item["target"] == 5349 and item["protocol"] == "tcp")]
        self.assert_code("LIVE_PORT_MISSING", acceptance.validate_live,
                         observations, PROJECT, model)
        model = rendered_model()
        observations = live_projection(model)
        media = next(item for item in observations if item["service"] == "media-plane")
        media["ports"] = [item for item in media["ports"]
                          if not (item["target"] == 40000 and item["host_ip"] == "0.0.0.0")]
        self.assertTrue(any(item["target"] == 40000 and item["host_ip"] == "::"
                            for item in media["ports"]))
        self.assert_code("LIVE_PORT_MISSING", acceptance.validate_live,
                         observations, PROJECT, model)

    def test_extra_data_publication(self):
        model = rendered_model()
        model["services"]["postgres"]["ports"].append(binding(5432, 5432, "tcp"))
        self.assert_code("RENDERED_PORT_EXTRA", acceptance.validate_rendered, model, PROJECT)
        model = rendered_model()
        observations = live_projection(model)
        db = next(item for item in observations if item["service"] == "postgres")
        db["ports"].append({"target": 5432, "published": 5432,
                            "protocol": "tcp", "host_ip": "0.0.0.0"})
        self.assert_code("LIVE_PORT_EXTRA", acceptance.validate_live,
                         observations, PROJECT, model)
        model = rendered_model()
        observations = live_projection(model)
        control = next(item for item in observations if item["service"] == "control-plane")
        control["ports"].append({"target": 8080, "published": 8080,
                                "protocol": "tcp", "host_ip": "::"})
        self.assert_code("LIVE_PORT_EXTRA", acceptance.validate_live,
                         observations, PROJECT, model)

    def test_invalid_range_or_protocol(self):
        model = rendered_model()
        model["services"]["media-plane"]["ports"][-1]["target"] = 42000
        self.assert_code("RENDERED_PORT_INVALID", acceptance.validate_rendered, model, PROJECT)
        model = rendered_model()
        model["services"]["coturn"]["ports"][0]["protocol"] = "sctp"
        self.assert_code("RENDERED_PORT_INVALID", acceptance.validate_rendered, model, PROJECT)
        # Compose's omitted and empty host_ip spellings both mean wildcard;
        # other falsey JSON values are malformed and must not coerce to it.
        model = rendered_model()
        model["services"]["media-plane"]["ports"][1]["host_ip"] = ""
        acceptance.validate_rendered(model, PROJECT)
        for invalid_host_ip in (None, False, 0, []):
            model = rendered_model()
            model["services"]["media-plane"]["ports"][1]["host_ip"] = invalid_host_ip
            with self.subTest(host_ip=invalid_host_ip):
                self.assert_code("RENDERED_PORT_INVALID", acceptance.validate_rendered,
                                 model, PROJECT)

    def test_duplicate_or_foreign_identity(self):
        observations = live_projection()
        observations[1]["id"] = observations[0]["id"]
        self.assert_code("LIVE_IDENTITY_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())
        observations = live_projection()
        observations[0]["project"] = "foreign-project"
        self.assert_code("LIVE_IDENTITY_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())
        observations = live_projection()
        observations[0]["name"] = "/foreign-container-name"
        self.assert_code("LIVE_IDENTITY_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())
        observations = live_projection()
        observations[1]["service"] = "foreign-service"
        self.assert_code("LIVE_SERVICE_SET_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())
        observations = live_projection()
        observations.pop()
        self.assert_code("LIVE_SERVICE_SET_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())

    def test_unhealthy_required_service(self):
        observations = live_projection()
        observations[0]["health"] = "starting"
        self.assert_code("LIVE_STATE_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())

    def test_running_minio_and_disabled_agent(self):
        observations = live_projection()
        minio = next(item for item in observations if item["service"] == "minio")
        agent = next(item for item in observations if item["service"] == "ops-agent")
        minio.update(state="running", health="none", exit_code=None)
        agent.update(state="exited", health="none", exit_code=0)
        verdict = acceptance.validate_live(observations, PROJECT, rendered_model())
        self.assertEqual(verdict["status"], "PASS")
        minio["state"] = "exited"
        self.assert_code("LIVE_STATE_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())

        observations = live_projection()
        agent = next(item for item in observations if item["service"] == "ops-agent")
        agent.update(state="running", health="none", exit_code=None)
        self.assert_code("LIVE_STATE_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())
        observations = live_projection()
        agent = next(item for item in observations if item["service"] == "ops-agent")
        agent.update(state="exited", health="none", exit_code=1)
        self.assert_code("LIVE_STATE_INVALID", acceptance.validate_live,
                         observations, PROJECT, rendered_model())

    def test_live_state_failure_carries_only_validated_fixed_service(self):
        for service in SERVICES:
            with self.subTest(service=service):
                observations = live_projection()
                selected = next(item for item in observations if item["service"] == service)
                selected["state"] = "PRIVATE_INVALID_STATE_SENTINEL"
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    acceptance.validate_live(observations, PROJECT, rendered_model())
                self.assertEqual(caught.exception.code, "LIVE_STATE_INVALID")
                self.assertEqual(caught.exception.service, service)
                self.assertIsNone(caught.exception.returncode)
                self.assertNotIn("PRIVATE_INVALID_STATE_SENTINEL", str(caught.exception))

    def test_partial_ipv6_bindings_rejected_but_absent_ipv6_is_allowed(self):
        model = rendered_model()
        observations = live_projection(model)
        for container in observations:
            container["ports"] = [port for port in container["ports"]
                                  if port["host_ip"] != "::"]
        # IPv6 is an observed parity requirement only when Docker reports it.
        verdict = acceptance.validate_live(observations, PROJECT, model)
        self.assertEqual(verdict["status"], "PASS")

        observations = live_projection(model)
        media = next(item for item in observations if item["service"] == "media-plane")
        media["ports"] = [port for port in media["ports"]
                          if not (port["host_ip"] == "::" and port["target"] == 40000)]
        self.assertTrue(any(port["host_ip"] == "0.0.0.0" and port["target"] == 40000
                            for port in media["ports"]))
        self.assertTrue(any(port["host_ip"] == "::" and port["target"] != 40000
                            for port in media["ports"]))
        self.assert_code("LIVE_PORT_MISSING", acceptance.validate_live,
                         observations, PROJECT, model)

    def test_wrong_or_writable_coturn_mount(self):
        model = rendered_model()
        model["services"]["coturn"]["volumes"][1]["source"] = "/tmp/wrong-coturn-certs"
        self.assert_code("RENDERED_MOUNT_INVALID", acceptance.validate_rendered, model, PROJECT)
        model = rendered_model()
        model["services"]["coturn"]["volumes"][1]["read_only"] = False
        self.assert_code("RENDERED_MOUNT_INVALID", acceptance.validate_rendered, model, PROJECT)
        model = rendered_model()
        model["services"]["coturn"]["volumes"].append({
            "type": "bind", "source": "/tmp/second-coturn-cert-source",
            "target": "/etc/coturn/certs", "read_only": True,
        })
        self.assert_code("RENDERED_MOUNT_INVALID", acceptance.validate_rendered, model, PROJECT)
        model = rendered_model()
        observations = live_projection(model)
        turn = next(item for item in observations if item["service"] == "coturn")
        cert_mount = next(mount for mount in turn["mounts"]
                          if mount["target"] == "/etc/coturn/certs")
        cert_mount["source"] = "/tmp/untrusted-certs"
        self.assert_code("LIVE_MOUNT_INVALID", acceptance.validate_live,
                         observations, PROJECT, model)
        observations = live_projection(model)
        turn = next(item for item in observations if item["service"] == "coturn")
        cert_mount = next(mount for mount in turn["mounts"]
                          if mount["target"] == "/etc/coturn/certs")
        cert_mount["read_only"] = False
        self.assert_code("LIVE_MOUNT_INVALID", acceptance.validate_live,
                         observations, PROJECT, model)
        observations = live_projection(model)
        turn = next(item for item in observations if item["service"] == "coturn")
        cert_mount = next(mount for mount in turn["mounts"]
                          if mount["target"] == "/etc/coturn/certs")
        cert_mount["type"] = "volume"
        self.assert_code("LIVE_MOUNT_INVALID", acceptance.validate_live,
                         observations, PROJECT, model)

    def test_evidence_refuses_symlink_and_unsafe_fields(self):
        safe = {"schema": 1, "candidate_sha": "a" * 40, "run_id": "run-17",
                "runner_os": "ubuntu-24.04", "tool_versions": {"docker": "29.8.2",
                                                                      "compose": "2.24.5"},
                "local_only": True,
                "checks": [{"stage": "observe", "assertion": "selected_stack",
                            "status": "PASS", "code": "OK"}],
                "public_checks": "UNVERIFIED"}
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp).resolve()
            out = acceptance.write_evidence(directory, safe)
            self.assertEqual(out.name, "evidence.json")
            self.assertTrue(out.is_file())
            decoded = json.loads(out.read_text(encoding="utf-8"))
            self.assertEqual(decoded, safe)
            sentinel = "SYNTHETIC_SECRET_SENTINEL_2505"
            unsafe = {**safe, "unexpected": sentinel}
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                acceptance.write_evidence(directory, unsafe)
            self.assertNotIn(sentinel, str(caught.exception))
            self.assertNotIn(sentinel, out.read_text(encoding="utf-8"))
            link_dir = directory / "linked"
            link_dir.mkdir()
            (link_dir / "evidence.json").symlink_to(out)
            self.assert_code("EVIDENCE_PATH_INVALID", acceptance.write_evidence,
                             link_dir, safe)
            for code in ("RENDERED_MODEL_INVALID", "RENDERED_SERVICE_INVALID"):
                case_dir = directory / code.lower()
                case_dir.mkdir()
                failure_record = {**safe, "checks": [{"stage": "render",
                    "assertion": "selected_stack", "status": "FAIL", "code": code}]}
                artifact = acceptance.write_evidence(case_dir, failure_record)
                self.assertEqual(json.loads(artifact.read_text(encoding="utf-8")), failure_record)
            unknown_dir = directory / "unknown-code"
            unknown_dir.mkdir()
            unknown_record = {**safe, "checks": [{"stage": "render",
                "assertion": "selected_stack", "status": "FAIL", "code": "UNEXPECTED_CODE"}]}
            self.assert_code("EVIDENCE_INVALID", acceptance.write_evidence,
                             unknown_dir, unknown_record)



class ProtectedMainWorkflowTrustTests(unittest.TestCase):
    """Execute the committed dispatch gate against owned command stubs only."""

    ALPHA = "a" * 40
    MAIN = "b" * 40
    GATE = "Verify protected main and pinned Alpha ancestry"
    CHECKOUT = "Checkout pinned protected Alpha"

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="owned-protected-main-gate-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.trace = self.root / "calls.jsonl"
        self.config = self.root / "responses.json"
        self.source = (ROOT / ".github/workflows/selfhost-acceptance.yml").read_text(encoding="utf-8")
        self.responses = {"protected": "true", "protected_rc": 0, "main": self.MAIN,
                          "main_rc": 0, "head": self.MAIN, "head_rc": 0,
                          "origin": self.MAIN, "origin_rc": 0, "ancestor_rc": 0}
        self.env = {"PATH": str(self.bin), "LANG": "C", "LC_ALL": "C",
                    "GITHUB_REPOSITORY": "Concord-Voice/Concord-Voice-Alpha",
                    "GITHUB_REF": "refs/heads/main", "GITHUB_SHA": self.MAIN, "ALPHA_SHA": self.ALPHA,
                    "PUBLIC_SHA": "c" * 40, "BUNDLE_SHA256": "d" * 64,
                    "STAGE_GIT_BUNDLE_SHA256": "e" * 64, "STAGE_RUN_ID": "701",
                    "GH_TOKEN": "owned-inert-token", "OWNED_GATE_CONFIG": str(self.config),
                    "OWNED_GATE_TRACE": str(self.trace)}
        stub = "#!" + sys.executable + "\n" + '''import json, os, pathlib, sys
config = json.loads(pathlib.Path(os.environ["OWNED_GATE_CONFIG"]).read_text())
tool = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with pathlib.Path(os.environ["OWNED_GATE_TRACE"]).open("a") as out:
    out.write(json.dumps([tool, *args]) + "\\n")
if tool == "gh" and args[:3] == ["api", "repos/Concord-Voice/Concord-Voice-Alpha/branches/main", "--jq"] and len(args) == 4:
    key = {".protected": "protected", ".commit.sha": "main"}.get(args[3])
    if key is not None:
        print(config[key])
        raise SystemExit(config[key + "_rc"])
elif tool == "git" and args == ["rev-parse", "HEAD"]:
    print(config["head"])
    raise SystemExit(config["head_rc"])
elif tool == "git" and args == ["rev-parse", "origin/main"]:
    print(config["origin"])
    raise SystemExit(config["origin_rc"])
elif tool == "git" and args == ["merge-base", "--is-ancestor", os.environ["ALPHA_SHA"], "origin/main"]:
    raise SystemExit(config["ancestor_rc"])
raise SystemExit(97)
'''
        for name in ("gh", "git"):
            path = self.bin / name
            path.write_text(stub, encoding="utf-8")
            path.chmod(0o700)

    def step(self, name):
        found = re.search(r"(?ms)^      - name: " + re.escape(name) +
                          r"\n(?P<body>.*?)(?=^      - name:|\Z)", self.source)
        self.assertIsNotNone(found, "required workflow trust step: " + name)
        return found.group("body")

    def gate_script(self):
        found = re.search(r"(?ms)^        run: \|\n(?P<body>(?:^          [^\n]*\n|^\s*\n)+)",
                          self.step(self.GATE))
        self.assertIsNotNone(found, "trust gate must expose its actual executable block")
        return "\n".join(line[10:] if line.startswith(" " * 10) else ""
                         for line in found.group("body").splitlines()) + "\n"

    def invoke(self, env_changes=None, response_changes=None):
        env = dict(self.env, **(env_changes or {}))
        self.config.write_text(json.dumps(dict(self.responses, **(response_changes or {}))), encoding="utf-8")
        self.trace.unlink(missing_ok=True)
        result = subprocess.run([TEST_BASH, "--noprofile", "--norc", "-c", self.gate_script()],
                                cwd=self.root, env=env, text=True, capture_output=True, timeout=5)
        calls = [json.loads(line) for line in self.trace.read_text().splitlines()] if self.trace.exists() else []
        self.assertNotIn("owned-inert-token", result.stdout + result.stderr)
        return result, calls

    def expected_calls(self):
        branch = "repos/Concord-Voice/Concord-Voice-Alpha/branches/main"
        return [["gh", "api", branch, "--jq", ".protected"],
                ["gh", "api", branch, "--jq", ".commit.sha"],
                ["git", "rev-parse", "HEAD"],
                ["git", "rev-parse", "origin/main"],
                ["git", "merge-base", "--is-ancestor", self.ALPHA, "origin/main"]]

    def test_actual_protected_main_gate_accepts_verified_ancestor_in_order(self):
        result, calls = self.invoke()
        self.assertEqual(result.returncode, 0, "owned protected-main positive control failed")
        self.assertEqual(calls, self.expected_calls())

    def test_actual_gate_refuses_wrong_repository_ref_and_malformed_dispatch_inputs(self):
        mutants = [("GITHUB_REPOSITORY", "synthetic/foreign"), ("GITHUB_REF", "refs/pull/1/merge"),
                   ("GITHUB_REF", "refs/heads/synthetic"), ("ALPHA_SHA", "a" * 39),
                   ("ALPHA_SHA", "A" * 40), ("PUBLIC_SHA", "g" * 40),
                   ("PUBLIC_SHA", "c" * 41), ("BUNDLE_SHA256", "d" * 63),
                   ("BUNDLE_SHA256", "D" * 64), ("STAGE_GIT_BUNDLE_SHA256", "e" * 65),
                   ("STAGE_GIT_BUNDLE_SHA256", "E" * 64), ("STAGE_RUN_ID", "0"),
                   ("STAGE_RUN_ID", "0701"), ("STAGE_RUN_ID", "701x")]
        mutants += [(key, "") for key in ("ALPHA_SHA", "PUBLIC_SHA", "BUNDLE_SHA256",
                                          "STAGE_GIT_BUNDLE_SHA256", "STAGE_RUN_ID")]
        for key, value in mutants:
            with self.subTest(field=key, value=value):
                result, calls = self.invoke({key: value})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [], "malformed authority must refuse before command effects")

    def test_actual_gate_refuses_unprotected_or_failed_branch_apis(self):
        mutants = [({"protected": "false"}, 1), ({"protected": "null"}, 1),
                   ({"protected": "true", "protected_rc": 29}, 1),
                   ({"main_rc": 29}, 2), ({"main": "b" * 39}, 2),
                   ({"main": "B" * 40}, 2)]
        for responses, count in mutants:
            with self.subTest(responses=responses):
                result, calls = self.invoke(response_changes=responses)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, self.expected_calls()[:count])

    def test_actual_gate_refuses_divergent_local_main_and_nonancestor(self):
        for responses, count in (({"origin": "f" * 40}, 4), ({"origin_rc": 29}, 4),
                                 ({"ancestor_rc": 1}, 5)):
            with self.subTest(responses=responses):
                result, calls = self.invoke(response_changes=responses)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, self.expected_calls()[:count])

    def test_actual_gate_refuses_stale_dispatch_and_checkout_identity(self):
        for identity in ("", self.ALPHA):
            with self.subTest(dispatch_sha=identity):
                result, calls = self.invoke({"GITHUB_SHA": identity})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, self.expected_calls()[:3])
        for response in ({"head": self.ALPHA}, {"head_rc": 29}):
            with self.subTest(checkout=response):
                result, calls = self.invoke(response_changes=response)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, self.expected_calls()[:3])

    def test_dispatch_gate_dominates_pinned_checkout_and_privileged_consumers(self):
        gate_at = self.source.index("- name: " + self.GATE)
        pinned_at = self.source.index("- name: " + self.CHECKOUT)
        self.assertLess(self.source.index("- name: Checkout exact candidate"), gate_at)
        self.assertLess(gate_at, pinned_at)
        for name in ("Authenticate staging and prepare exact public bundle",
                     "Install public host prerequisites", "Run exact-checkout self-host acceptance"):
            self.assertLess(pinned_at, self.source.index("- name: " + name))
        for name in (self.GATE, self.CHECKOUT, "Authenticate staging and prepare exact public bundle"):
            block = self.step(name)
            self.assertRegex(block, r"(?m)^        if: github\.event_name == 'workflow_dispatch'\s*$")
            self.assertNotRegex(block, r"(?m)^        continue-on-error:")
        for name in ("Checkout exact candidate", self.CHECKOUT):
            block = self.step(name)
            self.assertRegex(block, r"(?m)^        uses: actions/checkout@[a-f0-9]{40}(?:\s+#.*)?$")
            self.assertRegex(block, r"(?m)^          persist-credentials: false\s*$")
            self.assertRegex(block, r"(?m)^          fetch-depth: 0\s*$")
        self.assertRegex(self.step(self.CHECKOUT), r"(?m)^          ref: \$\{\{ inputs\.alpha_sha \}\}\s*$")
        for env_name, input_name in (("ALPHA_SHA", "alpha_sha"), ("PUBLIC_SHA", "public_sha"),
                                     ("BUNDLE_SHA256", "bundle_sha256"),
                                     ("STAGE_GIT_BUNDLE_SHA256", "stage_git_bundle_sha256"),
                                     ("STAGE_RUN_ID", "stage_run_id")):
            self.assertRegex(self.step(self.GATE), r"(?m)^          " + env_name +
                             r": \$\{\{ inputs\." + input_name + r" \}\}\s*$")
        self.assertRegex(self.gate_script(), r"(?m)^set -euo pipefail$")
        self.assertNotRegex(self.source, r"(?m)^\s*(?:continue-on-error:\s*true|(?:contents|actions|id-token):\s*write)\s*$")



class PublicMirrorSourceWorkflowTrustTests(unittest.TestCase):
    """Publisher retains authenticated current tooling and admits Alpha as data."""

    ALPHA = ProtectedMainWorkflowTrustTests.ALPHA
    MAIN = ProtectedMainWorkflowTrustTests.MAIN
    GATE = "Validate pinned self-host source"
    step = ProtectedMainWorkflowTrustTests.step
    gate_script = ProtectedMainWorkflowTrustTests.gate_script
    invoke = ProtectedMainWorkflowTrustTests.invoke

    def setUp(self):
        ProtectedMainWorkflowTrustTests.setUp(self)
        self.source = (ROOT / ".github/workflows/publish-public-mirror.yml").read_text(encoding="utf-8")
        self.env.pop("GH_TOKEN")
        self.env["PMIRROR_SOURCE_SHA"] = self.ALPHA
        self.responses = {"ancestor_rc": 0}
        stub = "#!" + sys.executable + "\n" + '''import json, os, pathlib, sys
config = json.loads(pathlib.Path(os.environ["OWNED_GATE_CONFIG"]).read_text())
args = sys.argv[1:]
with pathlib.Path(os.environ["OWNED_GATE_TRACE"]).open("a") as out:
    out.write(json.dumps(["git", *args]) + "\\n")
if args[:1] == ["fetch"]:
    print("owned private origin has no persisted credentials", file=sys.stderr)
    raise SystemExit(128)
if args == ["merge-base", "--is-ancestor", os.environ["PMIRROR_SOURCE_SHA"], "origin/main"]:
    raise SystemExit(config["ancestor_rc"])
raise SystemExit(97)
'''
        (self.bin / "git").write_text(stub, encoding="utf-8")

    def expected_calls(self, candidate=None):
        candidate = candidate or self.ALPHA
        return [["git", "merge-base", "--is-ancestor", candidate, "origin/main"]]

    def test_pinned_source_uses_existing_main_without_unauthenticated_private_fetch(self):
        for candidate in (self.ALPHA, "c" * 40):
            with self.subTest(candidate=candidate):
                result, calls = self.invoke({"PMIRROR_SOURCE_SHA": candidate})
                self.assertEqual(result.returncode, 0,
                                 "source gate must use main fetched by the authenticated checkout")
                self.assertEqual(calls, self.expected_calls(candidate))
                self.assertFalse(any(call[1:2] == ["fetch"] for call in calls))
                self.assertFalse(any(call[1:2] == ["checkout"] for call in calls),
                                 "Alpha data must not replace authenticated current publisher tooling")

    def test_pinned_source_refuses_malformed_sha_before_git(self):
        for candidate in ("", "a" * 39, "a" * 41, "A" * 40, "g" * 40, "a" * 40 + "\n"):
            with self.subTest(candidate=candidate):
                result, calls = self.invoke({"PMIRROR_SOURCE_SHA": candidate})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [])

    def test_pinned_source_refuses_nonancestor_without_candidate_checkout(self):
        result, calls = self.invoke(response_changes={"ancestor_rc": 1})
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, self.expected_calls())

    def test_pinned_source_ancestor_check_failure_is_not_success(self):
        result, calls = self.invoke(response_changes={"ancestor_rc": 29})
        self.assertEqual(result.returncode, 29)
        self.assertEqual(calls, self.expected_calls())

    def test_pinned_source_gate_follows_full_checkout_without_persisted_credentials(self):
        job = re.search(r"(?ms)^  publish:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", self.source)
        self.assertIsNotNone(job)
        checkout = re.search(r"(?ms)^      - uses: actions/checkout@[a-f0-9]{40}(?:[ \t]+#[^\n]*)?\n"
                             r"(?P<body>.*?)(?=^      - |\Z)", job.group("body"))
        self.assertIsNotNone(checkout)
        self.assertLess(checkout.start(), job.group("body").index("- name: " + self.GATE))
        self.assertRegex(checkout.group("body"), r"(?m)^          persist-credentials: false\s*$")
        self.assertRegex(checkout.group("body"), r"(?m)^          fetch-depth: 0\s*$")
        gate = self.step(self.GATE)
        self.assertRegex(gate, r"(?m)^        if: github\.event_name == 'workflow_dispatch' && "
                              r"format\('\{0\}', inputs\.stage_selfhost\) == 'true'\s*$")
        self.assertRegex(gate, r"(?m)^          PMIRROR_SOURCE_SHA: \$\{\{ inputs\.source_sha \}\}\s*$")
        self.assertNotRegex(gate, r"(?m)^        continue-on-error:")
        self.assertRegex(self.gate_script(), r"(?m)^set -euo pipefail$")
        self.assertNotRegex(self.gate_script(), r"(?m)^\s*git\s+checkout(?:\s|$)")



class ManualDispatchVerifierRevisionTests(unittest.TestCase):
    """Regression #2506: spend parsed workflow paths against two owned revisions.

    The checkout action is a local Git checkout adapter; executable fixtures mark
    which revision supplied policy. No host provisioning or remote API runs.
    """

    def setUp(self):
        import yaml
        temporary = tempfile.TemporaryDirectory(prefix="owned-dispatch-revisions-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.repo = self.root / "repository"
        self.repo.mkdir()
        self.git = shutil.which("git")
        self.assertIsNotNone(self.git)
        self.git_command("init", "--quiet", "--initial-branch=main")
        self.write_revision("poisoned-candidate")
        self.alpha = self.commit_tree()
        self.write_revision("authenticated-run")
        self.main = self.commit_tree(self.alpha)
        self.git_command("update-ref", "refs/heads/main", self.main)
        workflow = yaml.safe_load((ROOT / ".github/workflows/selfhost-acceptance.yml").read_text())
        self.steps = workflow["jobs"]["fresh-host-selfhost-acceptance"]["steps"]

    def git_command(self, *args, cwd=None):
        environment = {"PATH": TEST_PATH, "LANG": "C", "LC_ALL": "C",
                       "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                       "GIT_AUTHOR_NAME": "Owned fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
                       "GIT_COMMITTER_NAME": "Owned fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
                       "GIT_AUTHOR_DATE": "2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00Z"}
        result = subprocess.run([self.git, *args], cwd=cwd or self.repo, env=environment,
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def commit_tree(self, parent=None):
        self.git_command("add", ".")
        tree = self.git_command("write-tree")
        return self.git_command("commit-tree", tree, *(["-p", parent] if parent else []), "-m", "Owned revision")

    def write_revision(self, origin):
        # Import companions at the script's own path, just as selfhost-stage
        # loads selfhost-bundle and selfhost-evidence. A one-file copy is unsafe.
        program = '''import argparse, importlib.util, json, os
from pathlib import Path
ORIGIN = %r
parser = argparse.ArgumentParser()
for flag in ("alpha-sha", "public-sha", "stage-run-id", "stage-git-bundle-sha256", "bundle-sha256", "out", "evidence-dir", "bundle-identity", "blocked-prerequisite"):
    parser.add_argument("--" + flag)
parser.add_argument("--fresh-host", action="store_true")
args = parser.parse_args()
name = Path(__file__).name
observation = {"program": name, "origin": ORIGIN, "cwd": str(Path.cwd()), "file": str(Path(__file__).resolve())}
if name == "selfhost-stage.py":
    for companion in ("selfhost-bundle.py", "selfhost-evidence.py"):
        spec = importlib.util.spec_from_file_location(companion, Path(__file__).with_name(companion))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        observation[companion] = module.ORIGIN
    out = Path(args.out)
    bundle = out / "public-bundle/concord-selfhost-1.2.3"
    bundle.mkdir(parents=True)
    (bundle / "candidate.txt").write_text(args.alpha_sha)
    # An archive-supplied verifier must also never become trusted policy.
    runner = bundle / "infrastructure/deploy/tests/selfhost-acceptance.py"
    runner.parent.mkdir(parents=True)
    runner.write_text(Path(__file__).parents[2].joinpath("infrastructure/deploy/tests/selfhost-acceptance.py").read_text().replace(ORIGIN, "public-bundle"))
    (out / "bundle-identity.json").write_text(json.dumps({"alpha_sha": args.alpha_sha, "public_sha": args.public_sha}))
elif name == "selfhost-acceptance.py":
    identity = json.loads(Path(args.bundle_identity).read_text())
    observation["candidate"] = Path("candidate.txt").read_text()
    assert observation["candidate"] == identity["alpha_sha"] == os.environ["ALPHA_SHA"]
    observation["mode"] = "blocked" if args.blocked_prerequisite else "fresh"
    # The poison can mint accepted evidence if the caller chooses its program.
    evidence = {"accepted": True, "verifier_origin": ORIGIN, "alpha_sha": identity["alpha_sha"]}
    Path(args.evidence_dir).joinpath("evidence.json").write_text(json.dumps(evidence))
with Path(os.environ["OWNED_WORKFLOW_TRACE"]).open("a") as trace:
    trace.write(json.dumps(observation) + "\\n")
if name == "selfhost-acceptance.py" and os.environ.get("OWNED_ACCEPTANCE_FAIL") == "1":
    raise SystemExit(17)
''' % origin
        for relative in ("scripts/public-mirror/selfhost-stage.py",
                         "infrastructure/deploy/tests/selfhost-acceptance.py",
                         "infrastructure/deploy/tests/selfhost-nginx-diagnostic.py"):
            path = self.repo / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(program)
        for name in ("selfhost-bundle.py", "selfhost-evidence.py"):
            (self.repo / "scripts/public-mirror" / name).write_text("ORIGIN = " + repr(origin) + "\n")

    def invoke_workflow(self, scenario):
        sandbox = self.root / scenario
        sandbox.mkdir()
        workspace, runner_temp = sandbox / "workspace", sandbox / "runner-temp"
        runner_temp.mkdir()
        bin_dir = sandbox / "bin"
        bin_dir.mkdir()
        (bin_dir / "python3").symlink_to(sys.executable)
        (bin_dir / "gh").write_text("#!" + sys.executable + '''
import os, sys
assert sys.argv[1:4] == ["api", "repos/Concord-Voice/Concord-Voice-Alpha/branches/main", "--jq"]
assert len(sys.argv) == 5
print({".protected": "true", ".commit.sha": os.environ["GITHUB_SHA"]}[sys.argv[4]])
''')
        (bin_dir / "gh").chmod(0o700)
        trace = sandbox / "trace.jsonl"
        env = {"PATH": str(bin_dir) + os.pathsep + os.defpath, "LANG": "C", "LC_ALL": "C",
               "GITHUB_WORKSPACE": str(workspace), "RUNNER_TEMP": str(runner_temp),
               "GITHUB_REPOSITORY": "Concord-Voice/Concord-Voice-Alpha", "GITHUB_REF": "refs/heads/main",
               "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_SHA": self.main,
               "ALPHA_SHA": self.alpha, "PUBLIC_SHA": "c" * 40, "BUNDLE_SHA256": "d" * 64,
               "STAGE_GIT_BUNDLE_SHA256": "e" * 64, "STAGE_RUN_ID": "701",
               "OWNED_WORKFLOW_TRACE": str(trace), "OWNED_ACCEPTANCE_FAIL": "1" if scenario == "acceptance-failure" else "0",
               "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull}
        context = {"github.event_name": "workflow_dispatch", "github.sha": self.main,
                   "github.workspace": str(workspace), "runner.temp": str(runner_temp), "github.token": "owned-inert-token",
                   **{"inputs." + key: env[value] for key, value in
                      (("alpha_sha", "ALPHA_SHA"), ("public_sha", "PUBLIC_SHA"), ("bundle_sha256", "BUNDLE_SHA256"),
                       ("stage_git_bundle_sha256", "STAGE_GIT_BUNDLE_SHA256"), ("stage_run_id", "STAGE_RUN_ID"))}}

        def expand(value):
            return re.sub(r"\$\{\{\s*([^}]+?)\s*\}\}", lambda match: str(context[match[1].strip()]), str(value))

        failure, outcomes, reached = False, {step["id"]: "skipped" for step in self.steps if step.get("id")}, []
        for step in self.steps:
            name = step.get("name", step.get("uses", "unnamed"))
            condition = step.get("if")
            if condition:
                expression = str(condition).removeprefix("${{").removesuffix("}}")
                expression = expression.replace("failure()", repr(failure)).replace("always()", "True")
                for key, value in {**context, **{"steps." + key + ".outcome": value for key, value in outcomes.items()}}.items():
                    expression = expression.replace(key, repr(value))
                selected = SelfhostPathFilterPolicyTests.decide(expression, cancelled=False, ioc="success", detector="success", output="true")
            else:
                selected = not failure
            if not selected:
                continue
            if step.get("uses", "").startswith("actions/upload-artifact@"):
                break
            reached.append(name)
            if step.get("uses", "").startswith("actions/checkout@"):
                options = step.get("with", {})
                target = workspace / expand(options.get("path", "."))
                self.assertTrue(target.resolve().is_relative_to(workspace.resolve()))
                if target.exists():
                    shutil.rmtree(target)
                target.parent.mkdir(parents=True, exist_ok=True)
                self.git_command("clone", "--quiet", "--no-hardlinks", str(self.repo), str(target), cwd=sandbox)
                self.git_command("checkout", "--quiet", expand(options.get("ref", "${{ github.sha }}")), cwd=target)
                continue
            if name == "Install public host prerequisites":
                # The only external seam: model the prerequisite outcome without
                # executing sudo, apt, curl, service control, or host fixtures.
                outcomes[step["id"]] = "failure" if scenario == "host-failure" else "success"
                failure = scenario == "host-failure"
                continue
            self.assertIn("run", step, "unsupported workflow operation: " + name)
            step_env = dict(env, **{key: expand(value) for key, value in step.get("env", {}).items()})
            result = subprocess.run([TEST_BASH, "--noprofile", "--norc", "-c", expand(step["run"])],
                                    cwd=workspace / expand(step.get("working-directory", ".")), env=step_env,
                                    capture_output=True, text=True, timeout=10)
            expected_rc = 17 if name == "Run exact-checkout self-host acceptance" and scenario == "acceptance-failure" else 0
            self.assertEqual(result.returncode, expected_rc, "owned workflow setup failed at " + name + ": " + result.stderr)
            if step.get("id"):
                outcomes[step["id"]] = "failure" if result.returncode else "success"
            failure = failure or result.returncode != 0
        evidence = json.loads((runner_temp / "selfhost-acceptance-evidence/evidence.json").read_text())
        observations = [json.loads(line) for line in trace.read_text().splitlines()]
        return evidence, observations, reached

    def test_same_revision_dispatch_reaches_owned_verifiers_and_bundle(self):
        self.alpha = self.main
        evidence, observations, reached = self.invoke_workflow("success")
        self.assertIn("Verify protected main and pinned Alpha ancestry", reached)
        self.assertEqual([item["program"] for item in observations],
                         ["selfhost-stage.py", "selfhost-acceptance.py"])
        self.assertEqual([item["origin"] for item in observations], ["authenticated-run"] * 2)
        self.assertEqual(observations[1]["candidate"], self.main)
        self.assertEqual(evidence, {"accepted": True, "verifier_origin": "authenticated-run", "alpha_sha": self.main})

    def test_older_candidate_cannot_replace_authenticated_dispatch_verifiers(self):
        for scenario in ("success", "host-failure", "acceptance-failure"):
            with self.subTest(scenario=scenario):
                evidence, observations, reached = self.invoke_workflow(scenario)
                self.assertIn("Verify protected main and pinned Alpha ancestry", reached)
                self.assertEqual(self.git_command("merge-base", self.alpha, self.main), self.alpha)
                self.assertNotEqual(self.alpha, self.main, "positive control requires a genuinely older candidate")
                programs = [item["program"] for item in observations]
                expected = ["selfhost-stage.py", "selfhost-acceptance.py"]
                if scenario == "acceptance-failure":
                    expected.append("selfhost-nginx-diagnostic.py")
                self.assertEqual(programs, expected, "parsed workflow must reach every applicable verifier")
                self.assertEqual(observations[1]["candidate"], self.alpha,
                                 "install/bundle under test must remain the selected older candidate")
                self.assertEqual(observations[1]["mode"], "blocked" if scenario == "host-failure" else "fresh")
                self.assertTrue(evidence["accepted"], "poison positive control must be capable of minting evidence")
                self.assertEqual(evidence["alpha_sha"], self.alpha)
                origins = [item["origin"] for item in observations]
                origins.extend(observations[0][name] for name in ("selfhost-bundle.py", "selfhost-evidence.py"))
                self.assertEqual(origins, ["authenticated-run"] * len(origins),
                                 "candidate scripts replaced authenticated workflow-revision staging/admission or acceptance policy and minted release evidence")
                self.assertEqual(evidence["verifier_origin"], "authenticated-run")


    def publisher_steps(self):
        import yaml
        workflow = yaml.safe_load((ROOT / ".github/workflows/publish-public-mirror.yml").read_text())
        return workflow["jobs"]["selfhost-release"]["steps"]

    def publisher_identity_script(self):
        steps = self.publisher_steps()
        token = next(index for index, step in enumerate(steps)
                     if str(step.get("uses", "")).startswith("actions/create-github-app-token@"))
        guards = [step for step in steps[:token]
                  if "GITHUB_SHA" in step.get("run", "") and "HEAD" in step.get("run", "")]
        for step in guards:
            self.assertNotIn("if", step, "publisher identity must be checked unconditionally before App mint")
            self.assertNotIn("continue-on-error", step)
        return "\n".join(step["run"] for step in guards)

    def invoke_publisher_identity(self, expected, *, git_failure=False):
        with tempfile.TemporaryDirectory(dir=self.root) as directory:
            owned = Path(directory)
            env = {"PATH": TEST_PATH, "LANG": "C", "LC_ALL": "C",
                   "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                   "OWNED_TOKEN_MARKER": str(owned / "token"),
                   "OWNED_PUBLISH_MARKER": str(owned / "publisher")}
            if expected is not None:
                env["GITHUB_SHA"] = expected
            if git_failure:
                (owned / "git").write_text("#!/bin/sh\nexit 31\n")
                (owned / "git").chmod(0o700)
                env["PATH"] = str(owned) + os.pathsep + os.defpath
            # Only the parsed identity guard executes. These owned marker effects
            # represent reaching later boundaries; no App/API/publisher runs.
            script = self.publisher_identity_script() + '\nprintf token >"$OWNED_TOKEN_MARKER"\nprintf publisher >"$OWNED_PUBLISH_MARKER"\n'
            result = subprocess.run([TEST_BASH, "--noprofile", "--norc", "-euo", "pipefail", "-c", script],
                                    cwd=self.repo, env=env, text=True, capture_output=True, timeout=10)
            return result, (owned / "token").exists(), (owned / "publisher").exists()

    def test_selfhost_publisher_delayed_main_checks_out_exact_dispatch_revision(self):
        checkout = next(step for step in self.publisher_steps()
                        if str(step.get("uses", "")).startswith("actions/checkout@"))
        options = checkout.get("with", {})
        self.assertIs(options.get("persist-credentials"), False)
        self.assertEqual(options.get("fetch-depth"), 0)
        # The run was dispatched at alpha; protected main has since advanced.
        self.assertNotEqual(self.alpha, self.main)
        ref = options.get("ref", "${{ github.sha }}")
        selected = self.alpha if re.fullmatch(r"\$\{\{\s*github\.sha\s*\}\}", str(ref)) else ref
        self.git_command("checkout", "--quiet", selected)
        self.assertEqual(self.git_command("rev-parse", "HEAD"), self.alpha,
                         "approval delay must not select newer main as publisher policy")

    def test_selfhost_publisher_matching_head_can_reach_owned_effect_controls(self):
        result, token, publisher = self.invoke_publisher_identity(self.main)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(token and publisher, "matching checkout positive control did not reach both owned boundaries")

    def test_selfhost_publisher_identity_refuses_before_token_or_publish(self):
        for expected, git_failure in ((self.alpha, False), (None, False), ("", False),
                                      ("a" * 39, False), ("A" * 40, False),
                                      (self.main + "\n", False), (self.main, True)):
            with self.subTest(expected=expected, git_failure=git_failure):
                result, token, publisher = self.invoke_publisher_identity(expected, git_failure=git_failure)
                self.assertNotEqual(result.returncode, 0,
                                    "invalid or unavailable checkout identity reached pre-token boundary")
                self.assertFalse(token or publisher, "identity refusal must precede both owned effects")



class ManualSelfhostReleaseWorkflowTests(unittest.TestCase):
    """A costly release rehearsal requires an operator's explicit dispatch."""
    FILE = "selfhost-release-guards.yml"
    ROUTINE = ("pr-ci.yml", "main-ci.yml", "nightly-ci.yml", "main-cd.yml")
    # Independent inventory retains HEAD 2164d193 order, with canonical Source
    # validation and public nginx artifact regressions after the staged-candidate suite.
    COMMANDS = (
        "set -euo pipefail",
        "bash scripts/public-mirror/test-transform.sh",
        "bash scripts/public-mirror/test-assemble.sh",
        "bash scripts/public-mirror/test-publish-gates.sh",
        "bash scripts/public-mirror/test-mirror-release.sh",
        "bash scripts/public-mirror/test-selfhost-public-tree.sh",
        "python3 -m unittest scripts/public-mirror/test-selfhost-closure.py scripts/public-mirror/test-selfhost-bundle.py scripts/public-mirror/test-selfhost-evidence.py scripts/public-mirror/test-selfhost-stage.py scripts/public-mirror/test-selfhost-validator-provenance.py scripts/public-mirror/test-public-selfhost-nginx-artifact.py infrastructure/deploy/tests/test_selfhost_acceptance.py infrastructure/deploy/tests/test_selfhost_nginx_diagnostic.py [internal]tests/test_selfhost_nginx_fixture.py -v",
        "bash scripts/public-mirror/test-selfhost-compose.sh",
        "bash scripts/public-mirror/test-publish-selfhost-release.sh",
        "bash scripts/public-mirror/test-selfhost-operator-docs.sh",
    )

    def manual_source(self):
        path = ROOT / ".github/workflows" / self.FILE
        self.assertTrue(path.is_file(), "full release guards must exist as an explicit manual workflow")
        return path.read_text(encoding="utf-8")

    def manual(self):
        import yaml
        return yaml.safe_load(self.manual_source())

    def assert_manual_trigger(self, workflow):
        # PyYAML's YAML 1.1 parser represents the unquoted GitHub key 'on' as True.
        events = workflow.get("on", workflow.get(True))
        self.assertIsInstance(events, dict, "manual release trigger must be explicit")
        self.assertEqual(set(events), {"workflow_dispatch"},
                         "full release guards must never run on automatic or reusable-workflow events")

    def assert_inventory(self, workflow):
        job = workflow["jobs"]["selfhost-release-guards"]
        gates = [step for step in job["steps"] if step.get("name") == "Hermetic public self-host release gates"]
        self.assertEqual(len(gates), 1, "one complete release-suite execution is required")
        step = gates[0]
        self.assertNotIn("if", step, "suite execution must retain implicit step success()")
        self.assertNotIn("continue-on-error", step)
        commands = tuple(line.strip() for line in step["run"].splitlines()
                         if line.strip() and not line.lstrip().startswith("#"))
        self.assertEqual(commands, self.COMMANDS, "all original release commands must execute once in baseline order")

    def assert_ioc_admission(self, workflow):
        ioc, job = workflow["jobs"]["ioc"], workflow["jobs"]["selfhost-release-guards"]
        self.assertEqual(ioc.get("uses"), "./.github/workflows/ioc-scanning.yml")
        self.assertNotIn("steps", ioc, "IOC must use the established scanner, before repository commands")
        self.assertEqual(ioc.get("permissions"), {"contents": "read", "pull-requests": "read"})
        self.assertEqual(job.get("needs"), ["ioc"], "release execution requires the actual IOC dependency edge")
        condition = job.get("if", "")
        self.assertTrue(condition, "IOC refusal must be explicit")
        for cancelled in (False, True):
            for verdict in ("success", "failure", "skipped", "cancelled"):
                admitted = SelfhostPathFilterPolicyTests.decide(
                    condition, cancelled=cancelled, ioc=verdict, detector="success", output="true")
                self.assertEqual(admitted, not cancelled and verdict == "success",
                                 "failed, skipped, or cancelled IOC cannot reach any release step: "
                                 + repr((cancelled, verdict)))
        steps = job["steps"]
        self.assertTrue(str(steps[0].get("uses", "")).startswith("actions/checkout@"))
        installer = [index for index, step in enumerate(steps)
                     if step.get("name") == "Ensure PyYAML for the release gates"]
        gates = [index for index, step in enumerate(steps)
                 if step.get("name") == "Hermetic public self-host release gates"]
        self.assertEqual(len(installer), 1)
        self.assertEqual(len(gates), 1)
        self.assertLess(0, installer[0], "checkout must follow IOC and precede repository dependency installation")
        self.assertLess(installer[0], gates[0])
        for step in steps:
            self.assertNotIn("continue-on-error", step)
            self.assertNotIn("if", step, "steps must not run after prerequisite failure")

    def routines(self):
        import yaml
        return {path.name: yaml.safe_load(path.read_text(encoding="utf-8"))
                for path in (ROOT / ".github/workflows").glob("*.yml")}

    def assert_routines_do_not_invoke(self, workflows):
        # Follow actual local reusable calls, so moving an automatic invocation
        # one workflow deeper cannot make this policy assertion pass.
        queue, visited = list(self.ROUTINE), set()
        while queue:
            name = queue.pop()
            if name in visited:
                continue
            visited.add(name)
            for job_name, job in workflows[name]["jobs"].items():
                self.assertNotEqual(job_name, "selfhost-release-guards",
                                    "routine CI must not contain the full release job: " + name)
                uses = str(job.get("uses", "")).split("@", 1)[0]
                self.assertFalse(uses.endswith("/" + self.FILE),
                                 "routine CI must not call the manual release workflow: " + name)
                if uses.startswith("./.github/workflows/"):
                    queue.append(uses.rsplit("/", 1)[1])
                for step in job.get("steps", []):
                    run = "\n".join(line for line in step.get("run", "").splitlines()
                                    if not line.lstrip().startswith("#"))
                    self.assertNotIn(self.FILE, run,
                                     "routine CI must not dispatch release guards through gh/API: " + name)
                    for command in self.COMMANDS:
                        if command.startswith("bash "):
                            self.assertNotIn(command[5:], run,
                                             "routine CI must not execute the full release-suite constituent: " + name)

    def test_release_workflow_requires_explicit_manual_dispatch(self):
        self.assert_manual_trigger(self.manual())

    def test_release_suite_is_absent_from_routine_orchestrators_and_their_callees(self):
        self.assert_routines_do_not_invoke(self.routines())

    def test_manual_release_executes_every_original_suite_in_order(self):
        self.assert_inventory(self.manual())

    def test_ioc_success_controls_checkout_dependency_install_and_release_execution(self):
        self.assert_ioc_admission(self.manual())

    def runtime_job(self):
        jobs = self.manual()["jobs"]
        runtime = [job for name, job in jobs.items() if name not in ("ioc", "selfhost-release-guards")]
        self.assertEqual(len(runtime), 1,
                         "complete deployment and lifecycle runtime inventory requires a concurrent manual job")
        return runtime[0]

    def test_complete_runtime_job_runs_concurrently_with_original_release_after_ioc(self):
        workflow = self.manual()
        self.assert_manual_trigger(workflow)
        self.assert_inventory(workflow)
        self.assert_ioc_admission(workflow)
        for job in (workflow["jobs"]["selfhost-release-guards"], self.runtime_job()):
            self.assertEqual(job.get("needs"), ["ioc"], "release and runtime jobs must independently depend on IOC")
            self.assertEqual(job.get("runs-on"), "blacksmith-4vcpu-ubuntu-2404")
            self.assertEqual(job.get("timeout-minutes"), 40)
            self.assertEqual(job.get("permissions"), {"contents": "read"})
            for cancelled in (False, True):
                for verdict in ("success", "failure", "skipped", "cancelled"):
                    self.assertEqual(SelfhostPathFilterPolicyTests.decide(
                        job.get("if", ""), cancelled=cancelled, ioc=verdict, detector="success", output="true"),
                        not cancelled and verdict == "success")
        checkout = self.runtime_job()["steps"][0]
        self.assertTrue(str(checkout.get("uses", "")).startswith("actions/checkout@"))
        self.assertIs(checkout.get("with", {}).get("persist-credentials"), False)
        self.assertRegex(checkout.get("with", {}).get("ref", ""), r"^\$\{\{\s*github\.sha\s*\}\}$")

    def invoke_complete_runtime_inventory(self, *, failure=None, failure_group="deployment", rendezvous=False):
        import shlex
        job = self.runtime_job()
        # The independent oracle comes from tracked inputs, never the workflow's
        # list/glob. No actual repository test or provisioning script executes.
        git_env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        tracked = subprocess.check_output(["git", "-C", str(ROOT), "ls-files", "--",
                                           "infrastructure/deploy/tests/test-*.sh"], env=git_env, text=True).splitlines()
        inventory = tuple(tracked) + SelfhostPathFilterPolicyTests.LIFECYCLE_QUEUE_INVENTORY
        state = {"started": [], "completed": [], "active": [], "maximum": 0,
                 "paired": [], "unpaired": [], "pair_overlap": False,
                 "capacity_notified": [], "capacity_expired": []}
        with tempfile.TemporaryDirectory(prefix="owned-manual-runtime-inventory-") as directory:
            root = Path(directory)
            control = root / "control"
            control.mkdir()
            (control / "state.json").write_text(json.dumps(state))
            (control / "config.json").write_text(json.dumps({
                "inventory": inventory, "rendezvous": rendezvous,
                "failing": (inventory[-3] if failure_group == "lifecycle" else inventory[0])
                if failure is not None else None, "failure": failure}))
            consumer = control / "consumer.py"
            consumer.write_text(SelfhostPathFilterPolicyTests.QUEUE_CONSUMER)
            for name in inventory:
                target = root / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text("#!/bin/bash\nexec " + shlex.quote(sys.executable) + " " +
                                  shlex.quote(str(consumer)) + ' "$PWD/' + name + '"\n')
            descriptors = []
            try:
                for phase in ("pair", "capacity"):
                    for number in (1, 2):
                        fifo = control / (phase + "-" + str(number))
                        os.mkfifo(fifo)
                        descriptors.append(os.open(fifo, os.O_RDWR | os.O_NONBLOCK))
                results = []
                # Execute each copied queue independently, including its error
                # branches. This does not model later GitHub step admission
                # after an earlier step has failed.
                for step in job["steps"]:
                    script = step.get("run", "")
                    if "test-" not in script:
                        continue
                    self.assertNotIn("if", step, "full manual runtime inventory must not be path-filtered")
                    self.assertNotIn("continue-on-error", step)
                    env = {"PATH": TEST_PATH, "LC_ALL": "C", "OWNED_QUEUE_ROOT": str(control),
                           "RUN_SELFHOST_TESTS": "true"}
                    env.update({key: str(value) for key, value in step.get("env", {}).items()})
                    result = subprocess.run([TEST_BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script],
                                            cwd=root, env=env, capture_output=True, text=True, timeout=15)
                    results.append(result)
                state = json.loads((control / "state.json").read_text())
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)
        return results, state, inventory

    def test_complete_runtime_inventory_executes_every_tracked_deployment_and_lifecycle_consumer(self):
        results, state, inventory = self.invoke_complete_runtime_inventory(rendezvous=True)
        self.assertTrue(results, "manual runtime workflow has no executable test inventory")
        self.assertTrue(all(result.returncode == 0 for result in results), [result.stderr for result in results])
        self.assertCountEqual(state["started"], inventory, "manual runtime workflow dropped or repeated a tracked consumer")
        self.assertCountEqual(state["completed"], inventory)
        self.assertEqual(state["active"], [])
        self.assertEqual(state["maximum"], 2, "complete runtime inventory must use the reviewed two-worker queue")
        self.assertEqual((state["pair_overlap"], sorted(state["paired"]), state["unpaired"]), (True, [1, 2], []))

    def test_complete_runtime_inventory_aggregates_child_failure_without_dropping_consumers(self):
        for failure_group, failure in ((group, code) for group in ("deployment", "lifecycle")
                                       for code in (1, 255, "signal")):
            with self.subTest(queue=failure_group, child_failure=failure):
                results, state, inventory = self.invoke_complete_runtime_inventory(
                    failure=failure, failure_group=failure_group)
                self.assertTrue(any(result.returncode != 0 for result in results), "failed complete consumer did not fail the caller")
                self.assertCountEqual(state["started"], inventory)
                self.assertCountEqual(state["completed"], inventory)
                self.assertEqual(state["active"], [])
                self.assertLessEqual(state["maximum"], 2)

    def test_release_policy_rejects_automatic_triggers_and_invocations(self):
        actual = self.manual()
        self.assert_manual_trigger(actual)
        events_key = "on" if "on" in actual else True
        for event in ("pull_request", "push", "schedule", "workflow_call"):
            with self.subTest(event=event):
                mutant = copy.deepcopy(actual)
                mutant[events_key][event] = None
                with self.assertRaisesRegex(AssertionError, "automatic or reusable-workflow events"):
                    self.assert_manual_trigger(mutant)
        routines = self.routines()
        self.assert_routines_do_not_invoke(routines)
        for invocation in (
            {"uses": "./.github/workflows/" + self.FILE},
            {"steps": [{"run": "gh workflow run " + self.FILE}]},
            {"steps": [{"run": 'gh api --method POST "repos/$GITHUB_REPOSITORY/actions/workflows/' + self.FILE + '/dispatches"'}]},
        ):
            with self.subTest(invocation=invocation):
                mutant = copy.deepcopy(routines)
                mutant["build.yml"]["jobs"]["reintroduced-release"] = invocation
                with self.assertRaisesRegex(AssertionError, "routine CI must not (call|dispatch)"):
                    self.assert_routines_do_not_invoke(mutant)

    def test_release_policy_rejects_removed_suite_and_broken_ioc_edges(self):
        actual = self.manual()
        self.assert_inventory(actual)
        self.assert_ioc_admission(actual)
        for removed in self.COMMANDS[1:]:
            with self.subTest(removed=removed):
                mutant = copy.deepcopy(actual)
                step = next(step for step in mutant["jobs"]["selfhost-release-guards"]["steps"]
                            if step.get("name") == "Hermetic public self-host release gates")
                step["run"] = "\n".join(line for line in step["run"].splitlines() if line.strip() != removed)
                with self.assertRaisesRegex(AssertionError, "baseline order"):
                    self.assert_inventory(mutant)
        mutant = copy.deepcopy(actual)
        mutant["jobs"]["selfhost-release-guards"]["needs"] = []
        with self.assertRaisesRegex(AssertionError, "actual IOC dependency edge"):
            self.assert_ioc_admission(mutant)
        mutant = copy.deepcopy(actual)
        mutant["jobs"]["selfhost-release-guards"]["if"] = "${{ !cancelled() }}"
        with self.assertRaisesRegex(AssertionError, "cannot reach any release step"):
            self.assert_ioc_admission(mutant)



class SelfhostPathFilterPolicyTests(unittest.TestCase):
    CLASSIFICATION = "needs.detect-changes.result != 'success' || needs.detect-changes.outputs.selfhost != 'false'"
    CALLER = "!cancelled() && needs.ioc.result == 'success' && (" + CLASSIFICATION + ")"
    STATIC = "!cancelled() && needs.ioc.result == 'success'"
    STATIC_SELECTED = ("!inputs.enforcement-only && !cancelled() && needs.ioc.result == 'success' && "
                       "(needs.detect-changes.result != 'success' || needs.detect-changes.outputs.scan-known != 'true' || "
                       "fromJSON(needs.detect-changes.outputs.scan-selection).hotfix || "
                       "fromJSON(needs.detect-changes.outputs.scan-selection).advisory.static_guards || "
                       "needs.detect-changes.outputs.spa == 'true' || needs.detect-changes.outputs.services == 'true')")
    COSTLY = ("Run all test-*.sh under infrastructure/deploy/tests/",)

    def source(self):
        return (ROOT / ".github/workflows/pr-ci.yml").read_text(encoding="utf-8")

    def static_queue_step(self, workflow=None):
        import yaml
        workflow = yaml.safe_load(self.source()) if workflow is None else workflow
        steps = [step for step in workflow["jobs"]["static-guards"]["steps"]
                 if step.get("name") == self.COSTLY[0]]
        self.assertEqual(len(steps), 1, "one actual common deployment/lifecycle queue is required")
        return steps[0]

    def job(self, name):
        match = re.search(r"(?ms)^  " + re.escape(name) + r":\n(.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", self.source())
        self.assertIsNotNone(match, name)
        return "\n".join(line.split("#", 1)[0].rstrip() for line in match.group(1).splitlines())

    @staticmethod
    def condition(body, indent):
        lines = body.splitlines()
        for index, line in enumerate(lines):
            if line.startswith(" " * indent + "if:"):
                value = line.split("if:", 1)[1].strip()
                if value in {">-", ">", "|", "|-"}:
                    parts = []
                    for following in lines[index + 1:]:
                        if following.strip() and len(following) - len(following.lstrip()) <= indent:
                            break
                        parts.append(following.strip())
                    value = " ".join(parts)
                if value.startswith("${{") and value.endswith("}}"):
                    value = value[3:-2].strip()
                return " ".join(value.split())
        return None

    def filters(self):
        import yaml
        detector = self.job("detect-changes")
        self.assertRegex(detector, r"(?m)^      selfhost:\s*\$\{\{ steps\.filter\.outputs\.selfhost \}\}\s*$")
        self.assertIn(".github/ci/scan-relevance.yml", detector)
        policy = yaml.safe_load((ROOT / ".github/ci/scan-relevance.yml").read_text())
        patterns = policy.get("selfhost")
        self.assertIsInstance(patterns, list, "one shared selfhost dependency filter is required")
        self.assertEqual(patterns, policy.get("selfhost_guard"),
                         "the full public self-host dependency closure must select its actual fixtures")
        self.assertTrue(patterns)
        self.assertTrue(all(not pattern.startswith("!") for pattern in patterns),
                        "dependency admission uses additive simple path globs")
        return patterns

    @staticmethod
    def match_additive_paths_filter(path, patterns):
        """Closed subset of paths-filter's documented picomatch({dot:true}).

        Only the positive literal, *, ?, and whole-component ** forms used by
        this detector are admitted. Unlike whole-path fnmatch, * cannot cross
        '/'; dot:true lets all components match leading dots. Crosschecked with
        picomatch for the real detector and the published desktop inventory.
        """
        import fnmatch

        def consume(names, globs):
            if not globs:
                return not names
            if globs[0] == "**":
                return any(consume(names[offset:], globs[1:]) for offset in range(len(names) + 1))
            return (bool(names) and fnmatch.fnmatchcase(names[0], globs[0])
                    and consume(names[1:], globs[1:]))

        matches = []
        for pattern in patterns:
            if not isinstance(pattern, str) or not re.fullmatch(r"[A-Za-z0-9_./*?-]+", pattern):
                raise AssertionError("paths-filter pattern requires an expanded semantics adapter")
            parts = pattern.split("/")
            if any(part in {"", ".", ".."} or "**" in part and part != "**" for part in parts):
                raise AssertionError("paths-filter pattern is outside the closed component subset")
            matches.append(consume(path.split("/"), parts))
        return any(matches)

    def parsed_selfhost_filter(self):
        import yaml
        workflow = yaml.safe_load(self.source())
        detector = next(step for step in workflow["jobs"]["detect-changes"]["steps"] if step.get("id") == "filter")
        self.assertRegex(detector["uses"], r"^dorny/paths-filter@[0-9a-f]{40}$")
        self.assertIn(".github/ci/scan-relevance.yml", detector["with"]["filters"])
        self.assertEqual(detector["with"].get("predicate-quantifier"), "some-with-excludes")
        return workflow, self.filters()

    def test_mirror_published_desktop_subtree_uses_shared_application_lanes(self):
        # Desktop source, tests and build inputs use the ordinary desktop lane.
        # Their public-mirror presence alone does not require the extra installer
        # fixtures; that distinction is the approved self-host scope reduction.
        workflow, patterns = self.parsed_selfhost_filter()
        import yaml
        desktop_patterns = yaml.safe_load((ROOT / ".github/ci/scan-relevance.yml").read_text())["desktop"]
        manifest = (ROOT / "scripts/public-mirror/filter-manifest.txt").read_text()
        allows = [line.split()[1] for line in manifest.splitlines() if line.startswith("allow ")]
        inventory = subprocess.run(["git", "ls-files", "-z", "--", "client/desktop"], cwd=ROOT,
                                   capture_output=True, check=True, timeout=10).stdout.decode().split("\0")
        paths = [path for path in inventory if path]
        self.assertGreater(len(paths), 100, "published desktop positive control requires the actual tracked subtree")
        # New files in every published class must route the same way too.
        paths += ["client/desktop/src/renderer/components/FutureView.tsx",
                  "client/desktop/tests/unit/future.test.ts", "client/desktop/docs/future.md",
                  "client/desktop/vite.future.config.ts", "client/desktop/.future-config"]
        condition = self.static_queue_step(workflow)["env"]["RUN_SELFHOST_TESTS"]
        for path in paths:
            self.assertTrue(any(path == prefix or path.startswith(prefix + "/") for prefix in allows),
                            "fixture must be mirror-published before CI admission is tested: " + path)
            matched = self.match_additive_paths_filter(path, patterns)
            admitted = self.decide(condition, cancelled=False, ioc="success", detector="success",
                                   output="true" if matched else "false")
            self.assertFalse(matched or admitted, "desktop-only input must skip extra installer fixtures: " + path)
            self.assertTrue(self.match_additive_paths_filter(path, desktop_patterns),
                            "desktop-only input must keep its shared test lane: " + path)

    def test_parsed_filter_preserves_nonpublished_routing_and_routine_acceptance_retirement(self):
        workflow, patterns = self.parsed_selfhost_filter()
        for path, expected in (("client/desktop/package.json", False),
                               ("services/control-plane/internal/health/handler.go", False),
                               ("services/control-plane/Dockerfile", False),
                               ("infrastructure/deploy/install-selfhost.sh", True),
                               ("docker-compose.selfhost.yml", True),
                               ("scripts/public-mirror/transform.sh", True),
                               ("docs/internal/meeting-notes.md", False),
                               ("[internal]plans/private-plan.md", False), ("README.md", False)):
            with self.subTest(path=path):
                matched = self.match_additive_paths_filter(path, patterns)
                self.assertEqual(matched, expected, "parsed filter must preserve intended unrelated routing: " + path)
                self.assertEqual(self.decide(self.static_queue_step(workflow)["env"]["RUN_SELFHOST_TESTS"], cancelled=False,
                                             ioc="success", detector="success", output="true" if matched else "false"), expected)
        import yaml
        for name in ("pr-ci.yml", "main-ci.yml", "nightly-ci.yml", "main-cd.yml"):
            routine = yaml.safe_load((ROOT / ".github/workflows" / name).read_text())
            for job in routine["jobs"].values():
                self.assertFalse(str(job.get("uses", "")).split("@", 1)[0].endswith("/selfhost-acceptance.yml"),
                                 "full routine fresh-host acceptance caller must remain absent: " + name)
                for step in job.get("steps", []):
                    self.assertNotIn("infrastructure/deploy/tests/selfhost-acceptance.py", step.get("run", ""),
                                     "routine workflow must not execute fresh-host acceptance: " + name)

    def required_public_assembly_inputs(self):
        source = (ROOT / "scripts/public-mirror/test-assemble.sh").read_text(encoding="utf-8")
        arrays = re.findall(r"(?ms)^required_selfhost_paths=\(\n(.*?)^\)[ \t]*$", source)
        self.assertEqual(len(arrays), 1, "one actual required public assembly inventory is required")
        # Read the current literal array as data, never source or execute Bash.
        # A new shell expression must be reviewed rather than silently ignored.
        paths = arrays[0].split()
        self.assertTrue(paths, "required public assembly inventory must not be empty")
        self.assertEqual(len(paths), len(set(paths)), "required assembly paths must be unique")
        for path in paths:
            self.assertRegex(path, r"^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$")
            self.assertTrue(all(part not in {".", ".."} for part in path.split("/")), path)
        return paths

    def distinct_public_assembly_inputs(self):
        # Read the real publication inventory, then choose path classes whose
        # packaging/installer contracts are distinct from the SaaS app lanes.
        # This criterion is independent of the policy being tested: deleting a
        # required filter pattern cannot silently shrink the positive fixture.
        return [path for path in self.required_public_assembly_inputs()
                if path.startswith(("infrastructure/deploy/", "infrastructure/docker/", "docker-compose"))
                or path in {".env.example", "spa.env.example"}]

    def test_distinct_public_assembly_inputs_admit_selfhost_ci(self):
        import fnmatch
        patterns = self.filters()
        for path in self.distinct_public_assembly_inputs():
            with self.subTest(path=path):
                self.assertTrue(any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns),
                                "distinct public packaging input is outside self-host CI filter: " + path)

    def test_deleted_and_renamed_required_inputs_select_actual_guard_consumers(self):
        import fnmatch
        patterns = self.filters()
        import yaml
        parsed = yaml.safe_load(self.source())
        fixture_loop = self.static_queue_step(parsed)
        self.assertIn("RUN_SELFHOST_TESTS", fixture_loop.get("env", {}))
        conditions = {
            "deployment fixture loop": fixture_loop["env"]["RUN_SELFHOST_TESTS"],
            "lifecycle": fixture_loop["env"]["RUN_SELFHOST_TESTS"],
        }
        for condition in conditions.values():
            self.assertIsNotNone(condition)

        unrelated = "docs/internal/retired-release-input.txt"
        # Owned changed-path projections retain the former filename on a
        # rename. Removed inputs need no filesystem inode to select a guard.
        # These fixtures do not establish live GitHub changed-file discovery.
        cases = [("unrelated", unrelated, ((unrelated, "modified"),), False)]
        # Publication also consumes this private contract, which may be absent
        # until reviewed ingress evidence lands. Its introduction must select CI.
        required_inputs = (*self.distinct_public_assembly_inputs(), "infrastructure/ingress/contract.json")
        for path in required_inputs:
            cases.extend((
                ("modified", path, ((path, "modified"),), True),
                ("deleted", path, ((path, "deleted"),), True),
                ("renamed away", path, ((path, "deleted"), (unrelated, "added")), True),
                ("renamed into", path, ((unrelated, "deleted"), (path, "added")), True),
            ))
        for change, path, files, relevant in cases:
            with self.subTest(change=change, path=path):
                matched = any(fnmatch.fnmatchcase(filename, pattern)
                              for filename, _ in files for pattern in patterns)
                self.assertEqual(matched, relevant, "actual shared filter changed-path admission")
                output = "true" if matched else "false"
                for consumer, condition in conditions.items():
                    with self.subTest(consumer=consumer):
                        self.assertEqual(self.decide(condition, cancelled=False, ioc="success",
                                                     detector="success", output=output), relevant)

    def test_dependency_classes_admit_selfhost_ci(self):
        import fnmatch
        patterns = self.filters()
        distinct_paths = (
            "infrastructure/docker/coturn/turnserver.conf", "infrastructure/deploy/install-selfhost.sh",
            "infrastructure/ingress/contract.json",
            "infrastructure/deploy/concord-selfhost", "infrastructure/deploy/selfhost-security-event-logging.sh",
            "infrastructure/deploy/tests/selfhost-acceptance.py", "infrastructure/deploy/tests/test_selfhost_acceptance.py",
            "docker-compose.yml", "docker-compose.production.yml", "docker-compose.selfhost.yml",
            ".env.example", "infrastructure/deploy/deploy.env.example",
            "scripts/tests/test-concord-selfhost.sh", "scripts/tests/test-install-selfhost.sh",
            "scripts/tests/test-selfhost-log-tail-flags.sh",
            "scripts/public-mirror/transform.sh", "scripts/public-mirror/test-selfhost-stage.py",
            ".github/workflows/pr-ci.yml", ".github/workflows/selfhost-acceptance.yml",
            ".github/workflows/selfhost-release-guards.yml",
            ".github/requirements-pyyaml.txt",
            ".github/workflows/publish-public-mirror.yml",
        )
        shared_paths = (
            "services/control-plane/Dockerfile", "services/control-plane/go.mod",
            "services/control-plane/internal/health/handler.go", "services/media-plane/src/index.ts",
            "services/media-plane/package-lock.json", "client/admin/src/main.tsx",
            "client/desktop/package.json", "client/desktop/osv-scanner.toml",
            "docs/self-hosting/README.md", "docs/runbooks/selfhost-public-acceptance.md",
            "docs/GETTING_STARTED.md", "docs/architecture.md", ".gitignore",
        )
        for path in distinct_paths:
            with self.subTest(path=path):
                self.assertTrue(any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns), path)
        for path in shared_paths:
            with self.subTest(path=path):
                self.assertFalse(any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns), path)

    def test_nonpublished_private_planning_and_general_docs_skip_selfhost_ci(self):
        import fnmatch
        patterns = self.filters()
        for path in ("[internal]plans/private-plan.md", "[internal]specs/private-design.md",
                     "docs/internal/meeting-notes.md", "docs/architecture/unrelated.md", "README.md"):
            with self.subTest(path=path):
                self.assertFalse(any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns), path)

    def test_workflow_lint_runs_for_imported_python_policy_and_skips_unrelated_paths(self):
        import fnmatch
        import yaml

        def pattern_parts(pattern):
            # Closed adapter for the additive literal/*/?/** forms below.
            # Picomatch '*' never crosses '/', unlike Python's whole-path
            # fnmatch; '**' is the only component allowed to span directories.
            self.assertIsInstance(pattern, str)
            self.assertRegex(pattern, r"^[A-Za-z0-9_./*?-]+$")
            parts = pattern.split("/")
            self.assertTrue(all(part not in {"", ".", ".."} for part in parts))
            self.assertTrue(all("**" not in part or part == "**" for part in parts))
            return parts

        def matches(path, pattern):
            parts = pattern_parts(pattern)

            def consume(names, globs):
                if not globs:
                    return not names
                if globs[0] == "**":
                    return any(consume(names[offset:], globs[1:])
                               for offset in range(len(names) + 1))
                return (bool(names) and fnmatch.fnmatchcase(names[0], globs[0])
                        and consume(names[1:], globs[1:]))

            return consume(path.split("/"), parts)

        for path, pattern, expected in (
            (".github/scripts/tests/sonar_ioc_policy.py", ".github/scripts/*.py", False),
            (".github/scripts/tests/sonar_ioc_policy.py", ".github/scripts/tests/*.py", True),
            (".github/scripts/top_level.py", ".github/scripts/*.py", True),
            (".github/scripts/tests/nested/policy.py", ".github/scripts/tests/*.py", False),
            (".github/scripts/policy.py", ".github/scripts/**/policy.py", True),
            (".github/scripts/tests/nested/policy.py", ".github/scripts/**/policy.py", True),
        ):
            with self.subTest(matcher_path=path, pattern=pattern):
                self.assertEqual(matches(path, pattern), expected)

        workflow = yaml.safe_load(self.source())
        steps = workflow["jobs"]["workflow-lint"]["steps"]
        filters = [step for step in workflow["jobs"]["detect-changes"]["steps"]
                   if step.get("id") == "filter"]
        self.assertEqual(len(filters), 1)
        detector = filters[0]
        self.assertRegex(detector["uses"], r"^dorny/paths-filter@[0-9a-f]{40}$")
        self.assertIn(".github/ci/scan-relevance.yml", detector["with"]["filters"])
        patterns = yaml.safe_load((ROOT / ".github/ci/scan-relevance.yml").read_text())["workflow_lint"]
        self.assertTrue(patterns)
        for pattern in patterns:
            pattern_parts(pattern)

        # The shared selector now admits the whole job, so every lint step
        # consumes the same decision after one checkout and PyYAML install.
        condition = workflow["jobs"]["workflow-lint"]["if"]
        self.assertIn("needs.ioc.result == 'success'", condition)
        self.assertIn("advisory.workflow_lint", condition)
        self.assertFalse(any(step.get("id") == "filter" for step in steps))
        skip_name = "Fast-path skip (no workflow-relevant files changed)"
        self.assertFalse(any(step.get("name") == skip_name for step in steps))
        self_tests = next(step for step in steps if step.get("name") ==
                          "Self-test — lint scripts pass their own test suites")
        self.assertIn("bash .github/scripts/tests/sonar-required-gate.test.sh", self_tests["run"])
        self.assertGreater(len(steps), 1)
        for path, relevant in (
            (".github/scripts/tests/sonar_ioc_policy.py", True),
            (".github/scripts/tests/test_sonar_ioc_conditions.py", True),
            (".github/scripts/tests/future_policy.py", True),
            ("[internal]plans/private-plan.md", False),
            ("docs/internal/meeting-notes.md", False),
            (".github/examples/policy.py", False),
        ):
            with self.subTest(path=path):
                matched = any(matches(path, pattern) for pattern in patterns)
                self.assertEqual(matched, relevant, path)
                selected = self.decide(condition, cancelled=False, ioc="success",
                                       detector="success", output="false", advisory_workflow=matched)
                self.assertEqual(selected, relevant)
        self.assertTrue(self.decide(condition, cancelled=False, ioc="success", detector="failure",
                                    output="", advisory_workflow=False, previous_success=False),
                        "failed classification must run the full workflow lint")
        self.assertFalse(self.decide(condition, cancelled=False, ioc="failure", detector="success",
                                     output="true", advisory_workflow=True),
                         "workflow lint must not execute checkout code after IOC failure")

    @staticmethod
    def decide(expression, *, cancelled, ioc, detector, output, previous_success=True,
               known="true", hotfix=False, advisory_static=True, advisory_workflow=True,
               advisory_deployment=True, advisory_selfhost=True, spa=False, services=False,
               enforcement=False):
        # Parse the actual workflow condition through a closed Boolean subset.
        # No eval or subprocess executes workflow text. Step expressions lacking
        # a status function retain GitHub's implicit success() guard.
        import ast
        expression = expression.strip().removeprefix("${{").removesuffix("}}").strip()
        status_function = "cancelled()" in expression
        text = expression.replace("cancelled()", repr(cancelled))
        for token, value in (
            ("fromJSON(needs.detect-changes.outputs.scan-selection).advisory.static_guards", advisory_static),
            ("fromJSON(needs.detect-changes.outputs.scan-selection).advisory.workflow_lint", advisory_workflow),
            ("fromJSON(needs.detect-changes.outputs.scan-selection).advisory.deployment", advisory_deployment),
            ("fromJSON(needs.detect-changes.outputs.scan-selection).advisory.selfhost", advisory_selfhost),
            ("fromJSON(needs.detect-changes.outputs.scan-selection).hotfix", hotfix),
            ("needs.detect-changes.outputs.scan-known", known),
            ("needs.detect-changes.outputs.selfhost", output),
            ("needs.detect-changes.outputs.spa", "true" if spa else "false"),
            ("needs.detect-changes.outputs.services", "true" if services else "false"),
            ("needs.detect-changes.result", detector),
            ("needs.ioc.result", ioc),
            ("inputs.enforcement-only", enforcement),
        ):
            text = text.replace(token, repr(value))
        text = re.sub(r"!(?!=)", " not ", text).replace("&&", " and ").replace("||", " or ")
        def read(node):
            if isinstance(node, ast.Constant):
                return node.value
            if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
                return not read(node.operand)
            if isinstance(node, ast.BoolOp):
                values = [read(value) for value in node.values]
                if isinstance(node.op, ast.And):
                    return all(values)
                if isinstance(node.op, ast.Or):
                    return any(values)
            if isinstance(node, ast.Compare) and len(node.ops) == 1:
                left, right = read(node.left), read(node.comparators[0])
                if isinstance(node.ops[0], ast.Eq):
                    return left == right
                if isinstance(node.ops[0], ast.NotEq):
                    return left != right
            raise AssertionError("condition is outside the reviewed closed Boolean subset")
        result = read(ast.parse(text.strip(), mode="eval").body)
        return bool(result and (status_function or previous_success))

    def test_static_lifecycle_retains_conservative_classification_after_ioc_without_automatic_release_lane(self):
        static = self.job("static-guards")
        self.assertRegex(static, r"(?m)^    needs:\s*\[ioc, detect-changes\]\s*$")
        self.assertNotRegex(self.source(), r"(?m)^  selfhost-release-guards:",
                            "full release suite is reserved for explicit manual dispatch")
        self.assertEqual(self.condition(static, 4), self.STATIC_SELECTED)
        self.assertNotIn("outputs.selfhost", self.condition(static, 4))
        classified = []
        conditions = []
        for match in re.finditer(r"(?ms)^      - name: ([^\n]+)\n(.*?)(?=^      - (?:name:|uses:)|\Z)", static):
            name, body = match.groups()
            selector = re.search(r"(?m)^          RUN_SELFHOST_TESTS:\s*\$\{\{ (.*?) \}\}\s*$", body)
            if selector:
                classified.append(name)
                conditions.append(selector.group(1))
                self.assertNotRegex(body, r"(?m)^        continue-on-error:")
        self.assertEqual(sorted(classified), sorted(self.COSTLY))
        self.assertEqual(conditions, [self.CLASSIFICATION])
        # Two consumers remain: runner sizing and the common queue scope env.
        self.assertEqual(static.count("needs.detect-changes.outputs.selfhost"), 2)
        self.assertNotIn("Hermetic public self-host release gates", static)
        loop = re.search(r"(?ms)^      - name: Run all test-\*\.sh under infrastructure/deploy/tests/\n"
                         r"(.*?)(?=^      - name:|\Z)", static)
        self.assertIsNotNone(loop)
        loop_condition = self.condition(loop.group(1), 8)
        self.assertIn("advisory.deployment", loop_condition)
        self.assertIn("advisory.selfhost", loop_condition)
        self.assertFalse(self.decide(loop_condition, cancelled=False, ioc="success", detector="success",
                                     output="false", advisory_deployment=False, advisory_selfhost=False),
                         "unrelated changes must skip the complete deployment queue")
        for deployment, selfhost in ((True, False), (False, True)):
            self.assertTrue(self.decide(loop_condition, cancelled=False, ioc="success", detector="success",
                                        output="false", advisory_deployment=deployment,
                                        advisory_selfhost=selfhost),
                            "either affected deployment family must select the complete queue")
        self.assertTrue(self.decide(loop_condition, cancelled=False, ioc="success", detector="failure",
                                    output="", advisory_deployment=False, advisory_selfhost=False),
                        "failed classification must select the conservative complete queue")
        self.assertRegex(loop.group(1), r"(?m)^        env:\s*$")
        self.assertRegex(loop.group(1), r"(?m)^          RUN_SELFHOST_TESTS:\s*\$\{\{ " +
                         re.escape(self.CLASSIFICATION) + r" \}\}\s*$")
        self.assertEqual(len(re.findall(r"(?m)^          RUN_SELFHOST_TESTS:", static)), 1)
        for job in ("ioc", "semgrep", "secret-scanning", "govulncheck"):
            self.assertNotIn("outputs.selfhost", self.job(job), job)
        codeql_workflow = (ROOT / ".github/workflows/codeql-pr.yml").read_text(encoding="utf-8")
        codeql_job = re.search(r"(?ms)^  codeql:\n(.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", codeql_workflow)
        self.assertIsNotNone(codeql_job, "ordinary CodeQL caller must retain its CodeQL job")
        self.assertNotIn("outputs.selfhost", codeql_job.group(1),
                         "self-host classification must not suppress the native CodeQL caller")
        self.assertIn("uses: ./.github/workflows/codeql.yml", codeql_job.group(1),
                      "ordinary CodeQL caller must invoke the shared CodeQL workflow")
        # Self-host and workflow-lint classification share one detector. The
        # managed firewall fixture has a separate, narrowly scoped filter;
        # both must stay in detect-changes rather than execute in consumers.
        # Only the managed-firewall job may read that filter's result: a key in
        # scan-relevance.yml would join the protected scanner selection contract.
        import yaml
        jobs = yaml.safe_load(self.source())["jobs"]
        detectors = [(name, step.get("id"))
                     for name, job in jobs.items() for step in job.get("steps", [])
                     if str(step.get("uses", "")).startswith("dorny/paths-filter@")]
        self.assertCountEqual(detectors, [("detect-changes", "filter"),
                                          ("detect-changes", "firewall-filter")])
        # Contexts take dot or index syntax, and a whole object passed to a
        # function such as toJSON() counts as a read of everything in it.
        def member(name):
            return r"(?:\.%s\b|\[\s*['\"]%s['\"]\s*\])" % (re.escape(name), re.escape(name))
        whole = r"\s*\)"
        step_reads = re.compile(r"\bsteps(?:" + member("firewall-filter") + "|" + whole + ")")
        self.assertEqual(len(step_reads.findall(self.source())), 1)
        self.assertEqual(jobs["detect-changes"]["outputs"]["managed-firewall"],
                         "${{ steps.firewall-filter.outputs.managed_firewall }}")
        reader = re.compile(r"\bneeds(?:" + member("detect-changes") + "(?:" + member("outputs") +
                            "(?:" + member("managed-firewall") + "|" + whole + ")|" + whole + ")|" +
                            whole + ")")
        self.assertEqual([name for name in jobs if reader.search(self.job(name))], ["managed-firewall"])
        workflow_lint = self.job("workflow-lint")
        self.assertEqual(workflow_lint.count("uses: dorny/paths-filter@"), 0)
        self.assertIn("advisory.workflow_lint", workflow_lint)
        self.assertNotIn("outputs.selfhost", workflow_lint)

    def test_filter_failure_unknown_output_and_cancellation_decisions(self):
        static = self.job("static-guards")
        static_condition = self.condition(static, 4)
        self.assertIsNotNone(static_condition)
        step = self.static_queue_step()
        self.assertIn("RUN_SELFHOST_TESTS", step.get("env", {}))
        step_condition = step["env"]["RUN_SELFHOST_TESTS"]
        self.assertIsNotNone(step_condition)
        for cancelled in (False, True):
            for ioc in ("success", "failure", "skipped", "cancelled"):
                for detector in ("success", "failure", "skipped", "cancelled"):
                    for output in ("false", "true", "", "unknown"):
                        with self.subTest(cancelled=cancelled, ioc=ioc, detector=detector, output=output):
                            facts = dict(cancelled=cancelled, ioc=ioc, detector=detector, output=output)
                            safe_to_run = not cancelled and ioc == "success"
                            relevant = detector != "success" or output != "false"
                            self.assertEqual(self.decide(step_condition, **facts), relevant)
                            self.assertEqual(self.decide(static_condition, **facts), safe_to_run)
        self.assertTrue(self.decide(step.get("if", "True"), cancelled=False, ioc="success", detector="failure",
                                    output="", previous_success=False),
                        "selected deployment tests retain independent failure reporting")

    def test_manual_release_guards_has_reviewed_capacity_checkout_and_permissions(self):
        source = ManualSelfhostReleaseWorkflowTests().manual_source()
        match = re.search(r"(?ms)^  selfhost-release-guards:\n(.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", source)
        self.assertIsNotNone(match, "manual workflow must retain the complete release job")
        job = "\n".join(line.split("#", 1)[0].rstrip() for line in match.group(1).splitlines())
        self.assertRegex(job, r"(?m)^    needs:\s*\[ioc\]\s*$")
        self.assertEqual(self.condition(job, 4), self.STATIC)
        self.assertRegex(job, r"(?m)^    runs-on:\s*blacksmith-4vcpu-ubuntu-2404\s*$")
        self.assertRegex(job, r"(?m)^    timeout-minutes:\s*40\s*$")
        permissions = re.search(r"(?ms)^    permissions:\n((?:^      [^\n]*\n)+)", job)
        self.assertIsNotNone(permissions)
        self.assertEqual([line.strip() for line in permissions.group(1).splitlines()], ["contents: read"])
        self.assertNotRegex(job, r"(?m)^    (?:environment|secrets|uses):")
        self.assertNotRegex(job, r"(?m)^\s*continue-on-error:")
        checkout = re.search(r"(?ms)^      - uses: actions/checkout@[0-9a-f]{40}\s*\n(.*?)(?=^      - |\Z)", job)
        self.assertIsNotNone(checkout)
        self.assertRegex(checkout.group(1), r"(?m)^          persist-credentials:\s*false\s*$")
        self.assertRegex(checkout.group(1), r"(?m)^          fetch-depth:\s*0\s*$")
        dependency = "python3 -m pip install --user --quiet --disable-pip-version-check --require-hashes --only-binary=:all: -r .github/requirements-pyyaml.txt"
        self.assertIn(dependency, job)
        self.assertLess(job.index(dependency), job.index("- name: Hermetic public self-host release gates"))
        self.assertEqual(source.count("- name: Hermetic public self-host release gates"), 1)
        self.assertNotIn("- name: Hermetic public self-host release gates", self.source())

    def test_deployment_loop_skips_only_selfhost_tests_on_explicit_false_and_aggregates_failures(self):
        import yaml
        workflow = yaml.safe_load(self.source())
        step = next((item for item in workflow["jobs"]["static-guards"]["steps"]
                     if item.get("name") == "Run all test-*.sh under infrastructure/deploy/tests/"), None)
        self.assertIsNotNone(step)
        script = step.get("run")
        self.assertIsNotNone(script)
        # YAML preserves Bash ${#array[@]} and the worker heredoc verbatim;
        # the comment-stripping structural helper must not supply shell code.
        # Only inert owned test scripts exist at the workflow's actual glob.
        # Execute that copied run block, never repository deploy/host scripts.
        for classification in ("false", "true", "", "unknown", None):
            for failing in (None, "test-selfhost-acceptance.sh", "test-unrelated-deployment.sh"):
                with self.subTest(classification=classification, failing=failing):
                    with tempfile.TemporaryDirectory() as directory:
                        root = Path(directory)
                        tests = root / "infrastructure/deploy/tests"
                        tests.mkdir(parents=True)
                        names = ("test-selfhost-acceptance.sh", "test-selfhost-tls.sh",
                                 "test-unrelated-deployment.sh", "test-other-deployment.sh")
                        for name in names:
                            (tests / name).write_text(
                                "#!/usr/bin/env bash\n" + "printf '%s\\n' 'OWNED-RAN:" + name + "'\n" +
                                "exit " + ("1" if name == failing else "0") + "\n", encoding="utf-8")
                        for lifecycle_script in self.LIFECYCLE_QUEUE_INVENTORY:
                            path = root / lifecycle_script
                            path.parent.mkdir(parents=True, exist_ok=True)
                            path.write_text("#!/usr/bin/env bash\nprintf '%s\\n' 'OWNED-RAN:" + path.name + "'\n",
                                            encoding="utf-8")
                        env = {"PATH": TEST_PATH}
                        if classification is not None:
                            env["RUN_SELFHOST_TESTS"] = classification
                        result = subprocess.run(["bash", "-c", script], cwd=root, env=env,
                                                capture_output=True, text=True, timeout=10, check=False)
                        observed = [line.removeprefix("OWNED-RAN:") for line in result.stdout.splitlines()
                                    if line.startswith("OWNED-RAN:")]
                        expected = [name for name in names if classification != "false" or
                                    not name.startswith("test-selfhost-")]
                        if classification != "false":
                            expected += [Path(lifecycle_script).name for lifecycle_script in self.LIFECYCLE_QUEUE_INVENTORY]
                        self.assertCountEqual(observed, expected)
                        self.assertEqual(result.returncode, 1 if failing in expected else 0,
                                         "selected failures must aggregate while later selected tests still run")

    QUEUE_STEPS = COSTLY
    DEPLOY_QUEUE_INVENTORY = ("infrastructure/deploy/tests/test-a first.sh",
                              "infrastructure/deploy/tests/test-b'quoted.sh",
                              "infrastructure/deploy/tests/test-c\nline.sh",
                              "infrastructure/deploy/tests/test-d;literal.sh",
                              "infrastructure/deploy/tests/test-selfhost-acceptance.sh",
                              "infrastructure/deploy/tests/test-selfhost-tls.sh")
    LIFECYCLE_QUEUE_INVENTORY = ("scripts/tests/test-concord-selfhost.sh",
                                 "scripts/tests/test-install-selfhost.sh",
                                 "scripts/tests/test-provision-cert.sh")
    # A real executable consumer records complete script invocations, not shell
    # functions that disappear when the workflow launches a positional wrapper.
    QUEUE_CONSUMER = r'''import fcntl, json, os, pathlib, select, signal, sys
root = pathlib.Path(os.environ["OWNED_QUEUE_ROOT"])
name = pathlib.Path(sys.argv[1]).relative_to(pathlib.Path.cwd()).as_posix()
config = json.loads((root / "config.json").read_text())
def update(change):
    with (root / "state.json").open("r+") as state_file:
        fcntl.flock(state_file, fcntl.LOCK_EX)
        state = json.load(state_file)
        result = change(state)
        state_file.seek(0); json.dump(state, state_file); state_file.truncate()
        return result
def begin(state):
    assert name in config["inventory"], "workflow invoked an unexpected script consumer"
    assert name not in state["started"], "workflow repeated a script consumer"
    state["started"].append(name)
    state["active"].append(os.getpid())
    state["maximum"] = max(state["maximum"], len(state["active"]))
    ordinal = len(state["started"])
    if ordinal == 2:
        state["pair_overlap"] = len(state["active"]) == 2
    return ordinal
ordinal = update(begin)
if config["rendezvous"] and ordinal == 3:
    # The third independently live consumer is the only source of these bytes.
    # Keep both earlier consumers alive until this registration is observable.
    for number in (1, 2):
        release = os.open(root / ("capacity-" + str(number)), os.O_RDWR | os.O_NONBLOCK)
        os.write(release, b"third")
        os.close(release)
if config["rendezvous"] and ordinal <= 2:
    own = os.open(root / ("pair-" + str(ordinal)), os.O_RDWR | os.O_NONBLOCK)
    peer = os.open(root / ("pair-" + str(3 - ordinal)), os.O_RDWR | os.O_NONBLOCK)
    os.write(peer, b"ready")
    # A finite owned I/O handshake, not an elapsed-speed expectation. The
    # serial caller reaches the same consumers but its first read expires.
    ready = bool(select.select([own], [], [], 3)[0])
    if ready:
        os.read(own, 5)
    update(lambda state: state["paired" if ready else "unpaired"].append(ordinal))
    os.close(own); os.close(peer)
    # A pair acknowledgment proves the lower bound only. Hold both complete
    # consumers for a separate bounded I/O probe so a third available worker
    # registers before either can finish. With two slots there is no third
    # writer, so the finite read expires and releases the occupied slots.
    release = os.open(root / ("capacity-" + str(ordinal)), os.O_RDWR | os.O_NONBLOCK)
    third = bool(select.select([release], [], [], 3)[0])
    if third:
        os.read(release, 5)
    update(lambda state: state["capacity_notified" if third else "capacity_expired"].append(ordinal))
    os.close(release)
def finish(state):
    state["active"].remove(os.getpid())
    state["completed"].append(name)
update(finish)
if name == config["failing"]:
    if config["failure"] == "signal":
        os.kill(os.getpid(), signal.SIGTERM)
    raise SystemExit(config["failure"])
'''

    def invoke_static_script_queue(self, step_name, *, output="true", ioc="success",
                                   failure=None, rendezvous=False, observer_pair_control=False,
                                   observer_three_control=False):
        import shlex
        import yaml
        workflow = yaml.safe_load(self.source())
        job = workflow["jobs"]["static-guards"]
        self.assertEqual(job["timeout-minutes"], 60, "queue repair must preserve the approved static job budget")
        step = next(item for item in job["steps"] if item.get("name") == step_name)
        facts = dict(cancelled=False, ioc=ioc, detector="success", output=output)
        selected = self.decide(job["if"], **facts) and self.decide(step.get("if", "True"), **facts)
        inventory = self.DEPLOY_QUEUE_INVENTORY + self.LIFECYCLE_QUEUE_INVENTORY
        expected = [name for name in inventory if selected and
                    (output != "false" or name in self.DEPLOY_QUEUE_INVENTORY and
                     not Path(name).name.startswith("test-selfhost-"))]
        state = {"started": [], "completed": [], "active": [], "maximum": 0,
                 "paired": [], "unpaired": [], "pair_overlap": False,
                 "capacity_notified": [], "capacity_expired": []}
        if not selected:
            return SimpleNamespace(returncode=0, stdout="", stderr=""), state, expected
        with tempfile.TemporaryDirectory(prefix="owned-static-script-queue-") as directory:
            root = Path(directory)
            control = root / "control"
            control.mkdir()
            (control / "state.json").write_text(json.dumps(state))
            (control / "config.json").write_text(json.dumps(
                {"inventory": list(inventory), "rendezvous": rendezvous,
                 "failing": inventory[0] if failure is not None else None, "failure": failure}))
            consumer = control / "consumer.py"
            consumer.write_text(self.QUEUE_CONSUMER)
            # PATH contains a real owned executable so xargs and nested Bash
            # subprocesses exercise the same seam as the present direct caller.
            bin_dir = root / "bin"
            bin_dir.mkdir()
            (bin_dir / "bash").symlink_to(TEST_BASH)
            for name in (*inventory, "scripts/tests/test-unselected.sh",
                         "infrastructure/deploy/tests/nested/test-unselected.sh"):
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("#!/bin/bash\nexec " + shlex.quote(sys.executable) + " " +
                                shlex.quote(str(consumer)) + ' "$PWD/' + name.replace('"', '\\"') + '"\n')
            descriptors = []
            try:
                for phase in ("pair", "capacity"):
                    for number in (1, 2):
                        fifo = control / (phase + "-" + str(number))
                        os.mkfifo(fifo)
                        # Retain FIFO state even while the other consumer has not
                        # opened it; no harness-generated ready bytes are injected.
                        descriptors.append(os.open(fifo, os.O_RDWR | os.O_NONBLOCK))
                env = {"PATH": str(bin_dir) + os.pathsep + os.defpath, "LC_ALL": "C",
                       "OWNED_QUEUE_ROOT": str(control)}
                for key, value in step.get("env", {}).items():
                    env[key] = "true" if self.decide(value, **facts) else "false"
                if observer_pair_control or observer_three_control:
                    # Independently prove the fixture can SEE overlap: start
                    # two or three literal owned consumers, without a test queue or
                    # workflow rewrite. Regression cases always take the actual
                    # parsed run-block branch below.
                    count = 3 if observer_three_control else 2
                    pair = [subprocess.Popen([TEST_BASH, str(root / name)], cwd=root, env=env,
                                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                            for name in inventory[:count]]
                    for process in pair:
                        _, stderr = process.communicate(timeout=10)
                        self.assertEqual(process.returncode, 0, "owned overlap positive control failed: " + stderr)
                    result = SimpleNamespace(returncode=0, stdout="", stderr="")
                else:
                    result = subprocess.run([TEST_BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step["run"]],
                                            cwd=root, env=env, capture_output=True, text=True, timeout=15)
                state = json.loads((control / "state.json").read_text())
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)
        return result, state, expected

    def test_static_queue_owned_pair_control_observes_both_live_consumers(self):
        result, state, _ = self.invoke_static_script_queue(self.QUEUE_STEPS[0], rendezvous=True,
                                                           observer_pair_control=True)
        self.assertEqual(result.returncode, 0)
        self.assertCountEqual(state["started"], self.DEPLOY_QUEUE_INVENTORY[:2])
        self.assertCountEqual(state["completed"], self.DEPLOY_QUEUE_INVENTORY[:2])
        self.assertEqual(state["maximum"], 2, "fixture failed to observe both independently live processes")
        self.assertEqual((state["pair_overlap"], sorted(state["paired"]), state["unpaired"]), (True, [1, 2], []))
        self.assertEqual(state["active"], [])

    def test_static_queue_owned_three_consumer_control_observes_capacity(self):
        result, state, _ = self.invoke_static_script_queue(self.QUEUE_STEPS[0], rendezvous=True,
                                                           observer_three_control=True)
        self.assertEqual(result.returncode, 0)
        self.assertCountEqual(state["started"], self.DEPLOY_QUEUE_INVENTORY[:3])
        self.assertCountEqual(state["completed"], self.DEPLOY_QUEUE_INVENTORY[:3])
        self.assertEqual(state["maximum"], 3, "fixture failed to observe three independently live consumers")
        self.assertEqual((state["pair_overlap"], sorted(state["paired"]), state["unpaired"]), (True, [1, 2], []))
        self.assertEqual(sorted(state["capacity_notified"]), [1, 2], "both occupied slots must remain live until the third consumer registers")
        self.assertEqual(state["capacity_expired"], [])
        self.assertEqual(state["active"], [])

    def test_static_script_queues_rendezvous_three_and_never_exceed_three_consumers(self):
        # The real queue keeps three complete scripts live and never starts a
        # fourth. The third consumer must release the first pair's owned probe.
        for step_name in self.QUEUE_STEPS:
            with self.subTest(step=step_name):
                result, state, expected = self.invoke_static_script_queue(step_name, rendezvous=True)
                self.assertEqual(result.returncode, 0, "owned successful-script control failed: " + result.stderr)
                self.assertCountEqual(state["started"], expected, "queue must retain the complete selected inventory")
                self.assertCountEqual(state["completed"], expected, "every successful script must finish")
                self.assertEqual(state["active"], [], "caller returned before its script consumers completed")
                self.assertEqual(state["maximum"], 3, "the queue must use all three reviewed slots")
                self.assertEqual((state["pair_overlap"], sorted(state["paired"]), state["unpaired"]),
                                 (True, [1, 2], []),
                                 "first two independent complete script consumers must rendezvous concurrently")
                self.assertEqual(sorted(state["capacity_notified"]), [1, 2],
                                 "third consumer must enter while the first pair is live")
                self.assertEqual(state["capacity_expired"], [])

    def test_longest_deployment_consumer_starts_in_initial_wave(self):
        # Regression #2506: measured storage work starts late and finishes last.
        # Hold the first pair at the existing owned FIFO barriers, so their
        # recorded starts describe the initial occupied wave independently of
        # interpreter startup order or elapsed performance.
        storage = "infrastructure/deploy/tests/test-selfhost-storage.sh"
        deployment = (*self.DEPLOY_QUEUE_INVENTORY, storage)
        inventory = deployment + self.LIFECYCLE_QUEUE_INVENTORY
        with mock.patch.object(self, "DEPLOY_QUEUE_INVENTORY", deployment):
            result, state, expected = self.invoke_static_script_queue(
                self.QUEUE_STEPS[0], rendezvous=True)
            filtered_result, filtered_state, filtered_expected = self.invoke_static_script_queue(
                self.QUEUE_STEPS[0], output="false", failure=1)
        self.assertEqual(result.returncode, 0, "owned successful-script control failed: " + result.stderr)
        self.assertCountEqual(expected, inventory, "scheduling must preserve every discovered deployment script")
        self.assertCountEqual(state["started"], inventory, "scheduling dropped or repeated a deployment consumer")
        self.assertCountEqual(state["completed"], inventory, "every selected deployment script must finish")
        self.assertEqual(state["active"], [], "caller returned with a deployment consumer still active")
        self.assertEqual(state["maximum"], 3, "scheduling must preserve all three reviewed queue slots")
        self.assertEqual((state["pair_overlap"], sorted(state["paired"]), state["unpaired"]),
                         (True, [1, 2], []), "initial deployment wave was not held at the owned pair barrier")
        self.assertIn(storage, state["started"][:3],
                      "longest deployment consumer starts in initial wave: storage was queued behind earlier consumers")
        # The four unrelated filename fixtures remain selected; priority must
        # never reintroduce storage after the actual explicit-false filter.
        self.assertCountEqual(filtered_expected, inventory[:4])
        self.assertNotIn(storage, filtered_state["started"], "priority reintroduced explicitly skipped storage")
        self.assertEqual(filtered_result.returncode, 1, "filtered child failure must retain aggregate status one")
        self.assertCountEqual(filtered_state["started"], inventory[:4], "filter dropped or repeated an unrelated script")
        self.assertCountEqual(filtered_state["completed"], inventory[:4], "filtered failure abandoned another script")
        self.assertEqual(filtered_state["active"], [])
        self.assertLessEqual(filtered_state["maximum"], 3)

    def test_static_script_queues_preserve_filtered_inventory_and_aggregate_every_child_failure(self):
        for step_name in self.QUEUE_STEPS:
            for output in ("true", "false"):
                for failure in (None, 1, 255, "signal"):
                    with self.subTest(step=step_name, output=output, child_failure=failure):
                        result, state, expected = self.invoke_static_script_queue(step_name, output=output, failure=failure)
                        failed = failure is not None and bool(expected)
                        self.assertEqual(result.returncode == 0, not failed,
                                         "a failed complete script, including 255 or signal, must fail its caller")
                        self.assertCountEqual(state["started"], expected,
                                              "child failure must not drop another selected script consumer")
                        self.assertCountEqual(state["completed"], expected,
                                              "every selected case must still execute after child failure")
                        self.assertEqual(state["active"], [])
                        self.assertLessEqual(state["maximum"], 3)

    def test_static_script_queues_require_successful_ioc_before_any_consumer(self):
        for step_name in self.QUEUE_STEPS:
            for ioc in ("failure", "skipped", "cancelled"):
                with self.subTest(step=step_name, ioc=ioc):
                    result, state, expected = self.invoke_static_script_queue(step_name, ioc=ioc)
                    self.assertEqual(result.returncode, 0)
                    self.assertEqual(expected, [])
                    self.assertEqual(state["started"], [])
                    self.assertEqual(state["completed"], [])

    def test_protected_manual_bundle_dispatch_has_no_path_gate(self):
        reusable = (ROOT / ".github/workflows/selfhost-acceptance.yml").read_text(encoding="utf-8")
        self.assertRegex(reusable, r"(?m)^  workflow_dispatch:")
        self.assertNotIn("outputs.selfhost", reusable)
        self.assertNotIn("needs.detect-changes", reusable)
        self.assertNotRegex(reusable, r"(?m)^    if:")
        self.assertNotRegex(reusable, r"(?m)^\s+paths(?:-ignore)?:")
        self.assertNotRegex(self.source(), r"(?ms)^  pull_request:\n(?:(?!^  [A-Za-z0-9_-]+:).)*?^\s+paths(?:-ignore)?:")

    def test_blacksmith_runner_labels_and_four_core_acceptance_are_scoped(self):
        self.assertRegex(self.job("detect-changes"),
                         r"(?m)^    runs-on:\s*blacksmith-2vcpu-ubuntu-2404\s*$")
        reusable = (ROOT / ".github/workflows/selfhost-acceptance.yml").read_text(encoding="utf-8")
        job = re.search(r"(?ms)^  fresh-host-selfhost-acceptance:\n(.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", reusable)
        self.assertIsNotNone(job)
        self.assertRegex(job.group(1), r"(?m)^    runs-on:\s*blacksmith-4vcpu-ubuntu-2404\s*$")
        self.assertRegex(job.group(1), r"(?m)^    env:\s*$")
        self.assertRegex(job.group(1), r"(?m)^      SELFHOST_ACCEPTANCE_RUNNER_PROFILE:\s*blacksmith-4vcpu-ubuntu-2404\s*$")
        self.assertEqual(len(re.findall(r"(?m)^      SELFHOST_ACCEPTANCE_RUNNER_PROFILE:", job.group(1))), 1)
        self.assertRegex(job.group(1), r"(?m)^    timeout-minutes:\s*50\s*$")
        self.assertNotIn("blacksmith-8vcpu", job.group(1))
        self.assertNotRegex(job.group(1), r"(?m)^    if:")
        self.assertRegex(self.job("static-guards"), r"(?m)^    timeout-minutes:\s*60\s*$")

    def test_static_blacksmith_size_follows_actual_conservative_classification(self):
        import ast
        job = self.job("static-guards")
        match = re.search(r"(?m)^    runs-on:\s*(.+)$", job)
        self.assertIsNotNone(match)
        actual = match.group(1).strip()
        self.assertTrue(actual.startswith("${{") and actual.endswith("}}"))
        expression = actual[3:-2].strip()
        expected = "(" + self.CLASSIFICATION + ") && 'blacksmith-4vcpu-ubuntu-2404' || 'blacksmith-2vcpu-ubuntu-2404'"
        self.assertEqual(" ".join(expression.split()), expected)
        def read(node):
            if isinstance(node, ast.Constant) and isinstance(node.value, (str, bool)):
                return node.value
            if isinstance(node, ast.BoolOp):
                # GitHub returns an operand, rather than all()/any() booleans.
                result = None
                for value in node.values:
                    result = read(value)
                    if isinstance(node.op, ast.And) and not result:
                        return result
                    if isinstance(node.op, ast.Or) and result:
                        return result
                return result
            if isinstance(node, ast.Compare) and len(node.ops) == 1 and isinstance(node.ops[0], ast.NotEq):
                return read(node.left) != read(node.comparators[0])
            raise AssertionError("runner selection is outside the reviewed closed Boolean/string subset")
        for detector in ("success", "failure", "skipped", "cancelled"):
            for output in ("false", "true", "", "unknown"):
                with self.subTest(detector=detector, output=output):
                    text = expression.replace("needs.detect-changes.result", repr(detector))
                    text = text.replace("needs.detect-changes.outputs.selfhost", repr(output))
                    text = text.replace("&&", " and ").replace("||", " or ")
                    selected = read(ast.parse(text, mode="eval").body)
                    self.assertEqual(selected, "blacksmith-2vcpu-ubuntu-2404" if
                                     detector == "success" and output == "false" else
                                     "blacksmith-4vcpu-ubuntu-2404")



class AcceptanceWorkflowTests(unittest.TestCase):
    def test_desktop_mirror_dispatch_preserves_protected_main_publisher_isolation(self):
        source = (ROOT / ".github/workflows/build-desktop.yml").read_text(encoding="utf-8")
        caller = re.search(r"(?ms)^  public-release-mirror:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", source)
        self.assertIsNotNone(caller)
        active = "\n".join(line.split("#", 1)[0].rstrip() for line in caller.group("body").splitlines())
        self.assertRegex(active, r"(?m)^    needs:\s*\[detect-release, release-build, release\]\s*$")
        for guard in ("always()", "github.repository == 'Concord-Voice/Concord-Voice-Alpha'",
                      "github.event_name == 'workflow_dispatch'", "github.ref == 'refs/heads/main'",
                      "needs.release.result == 'success'"):
            self.assertIn(guard, active)
        permissions = re.search(r"(?ms)^    permissions:\n(?P<body>(?:^      [^\n]*\n)+)", active)
        self.assertIsNotNone(permissions)
        caller_grants = [line.strip() for line in permissions.group("body").splitlines()]
        required = {"actions: write", "checks: read"}
        self.assertEqual(set(caller_grants), required)
        self.assertEqual(len(caller_grants), len(required))
        self.assertRegex(active, r"(?m)^    runs-on:\s*ubuntu-latest\s*$")
        self.assertRegex(active, r"(?m)^    timeout-minutes:\s*45\s*$")
        self.assertNotRegex(active, r"(?m)^    (?:uses|secrets):")
        self.assertNotIn("secrets.", active)
        self.assertNotIn("PUBLIC_MIRROR_APP_PRIVATE_KEY", active)
        self.assertNotIn("CF_PAGES_DEPLOY_HOOK", active)
        self.assertNotRegex(active, r"(?m)^\s*continue-on-error:")
        self.assertIn("GH_TOKEN: ${{ github.token }}", active)
        self.assertIn("RELEASE_TAG: ${{ needs.release.outputs.release_tag }}", active)
        self.assertIn("set -euo pipefail", active)
        self.assertIn('RUN_ID=$(gh api --method POST', active)
        self.assertIn('"repos/$GITHUB_REPOSITORY/actions/workflows/publish-public-mirror.yml/dispatches"', active)
        self.assertIn("-F return_run_details=true", active)
        self.assertRegex(active, r"(?m)^\s+-f ref=main\s*\\?\s*$")
        self.assertIn('-f "inputs[mirror_release_tag]=$RELEASE_TAG"', active)
        self.assertEqual(re.findall(r"inputs\[([^]]+)\]", active), ["mirror_release_tag"])
        self.assertIn("--jq '.workflow_run_id'", active)
        self.assertIn('test -n "$RUN_ID"', active)
        self.assertIn('gh run watch "$RUN_ID" --repo "$GITHUB_REPOSITORY" --compact --exit-status', active)
        self.assertNotIn("gh run list", active, "await the dispatched run, never an inferred neighboring run")
        publisher = (ROOT / ".github/workflows/publish-public-mirror.yml").read_text(encoding="utf-8")
        self.assertNotRegex(publisher, r"(?m)^  workflow_call:",
                            "publisher execution must remain a separate protected-main run")

    def test_manual_workflow_preserves_existing_mirror_regressions_with_public_release_suites(self):
        source = ManualSelfhostReleaseWorkflowTests().manual_source()
        lane = re.search(r"(?ms)^  selfhost-release-guards:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", source)
        self.assertIsNotNone(lane, "hermetic release gates require their dedicated timeout budget")
        step = re.search(r"(?ms)^      - name: Hermetic public self-host release gates\n"
                         r"(?P<body>.*?)(?=^      - name:|\Z)", lane.group("body"))
        self.assertIsNotNone(step, "manual workflow must run the required mirror and release regressions")
        body = "\n".join(line.split("#", 1)[0].rstrip() for line in step.group("body").splitlines())
        self.assertRegex(body, r"(?m)^        run:\s*\|\s*$")
        self.assertEqual(SelfhostPathFilterPolicyTests.condition(lane.group("body"), 4),
                         SelfhostPathFilterPolicyTests.STATIC)
        self.assertIsNone(SelfhostPathFilterPolicyTests.condition(body, 8),
                          "IOC admission supplies safety; suite retains implicit step success()")
        self.assertNotRegex(body, r"(?m)^        continue-on-error:")
        commands = [line.strip() for line in body.splitlines() if line.startswith("          ") and line.strip()]
        self.assertIn("set -euo pipefail", commands)
        for name in ("test-transform.sh", "test-assemble.sh", "test-publish-gates.sh",
                     "test-mirror-release.sh", "test-selfhost-public-tree.sh",
                     "test-selfhost-compose.sh", "test-publish-selfhost-release.sh",
                     "test-selfhost-operator-docs.sh"):
            command = "bash scripts/public-mirror/" + name
            self.assertEqual(commands.count(command), 1,
                             f"required hermetic regression must execute exactly once: {name}")
        python_commands = [command for command in commands if command.startswith("python3 -m unittest ")]
        self.assertEqual(len(python_commands), 1)
        python_arguments = python_commands[0].split()
        for suite in ("scripts/public-mirror/test-selfhost-closure.py",
                      "scripts/public-mirror/test-selfhost-bundle.py",
                      "scripts/public-mirror/test-selfhost-evidence.py",
                      "scripts/public-mirror/test-selfhost-stage.py",
                      "infrastructure/deploy/tests/test_selfhost_acceptance.py"):
            self.assertEqual(python_arguments.count(suite), 1,
                             f"required hermetic Python suite must execute exactly once: {suite}")
        self.assertEqual(python_arguments[-1], "-v")
        self.assertFalse(any("|| true" in command for command in commands))

    def test_fresh_host_prerequisites_explicitly_install_required_udp_collector(self):
        source = (ROOT / ".github/workflows/selfhost-acceptance.yml").read_text(encoding="utf-8")
        job = re.search(r"(?ms)^  fresh-host-selfhost-acceptance:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", source)
        self.assertIsNotNone(job, "fresh-host acceptance job must provision its tools")
        active = "\n".join(line.split("#", 1)[0].rstrip() for line in job.group("body").splitlines())
        step = re.search(r"(?ms)^      - name: Install public host prerequisites\n"
                         r"(?P<body>.*?)(?=^      - name:|\Z)", active)
        self.assertIsNotNone(step, "actual prerequisite step must install the UDP collector")
        self.assertRegex(step.group("body"), r"(?m)^        run:\s*\|\s*$")
        packages = set()
        for line in step.group("body").splitlines():
            install = re.fullmatch(r"\s+sudo apt-get install --yes --no-install-recommends "
                                   r"(?P<packages>[a-z0-9.+-]+(?: [a-z0-9.+-]+)*)\s*", line)
            if install is not None:
                packages.update(install.group("packages").split())
        # Independent controls keep the existing prerequisite writer in view;
        # an image's incidental PATH/tool contents do not satisfy this contract.
        for package in ("logrotate", "nginx", "python3", "curl", "openssl", "ca-certificates", "gnupg"):
            with self.subTest(existing_package=package):
                self.assertIn(package, packages)
        self.assertIn("iproute2", packages,
                      "installer UDP preflight requires ss, provided by the explicit iproute2 package")

    def test_manual_acceptance_job_is_least_privilege_and_uploads_only_evidence(self):
        source = (ROOT / ".github/workflows/pr-ci.yml").read_text(encoding="utf-8")
        self.assertRegex(source, r"(?m)^  pull_request:")
        manual_workflow = (ROOT / ".github/workflows/selfhost-acceptance.yml").read_text(encoding="utf-8")
        self.assertRegex(manual_workflow, r"(?m)^  workflow_dispatch:")
        manual_job = re.search(r"(?ms)^  fresh-host-selfhost-acceptance:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:|\Z)", manual_workflow)
        self.assertIsNotNone(manual_job, "manual workflow must run the original fresh-host harness")
        active = "\n".join(line.split("#", 1)[0].rstrip() for line in manual_job.group("body").splitlines())
        self.assertRegex(active, r"(?m)^    runs-on:\s*blacksmith-4vcpu-ubuntu-2404\s*$")
        self.assertRegex(active, r"(?m)^    timeout-minutes:\s*50\s*$")
        step_limits = {"Checkout exact candidate": 1, "Create private evidence directory": 1,
                       "Install public host prerequisites": 2, "Record host prerequisite failure": 1,
                       "Run exact-checkout self-host acceptance": 44, "Upload sanitized acceptance evidence": 1}
        for step_name, minutes in step_limits.items():
            step = re.search(r"(?ms)^      - name: " + re.escape(step_name) +
                             r"\n(?P<body>.*?)(?=^      - name:|\Z)", active)
            self.assertIsNotNone(step, step_name)
            self.assertRegex(step.group("body"), r"(?m)^        timeout-minutes:\s*" + str(minutes) + r"\s*$")
        self.assertRegex(active, r"(?m)^      contents:\s*read\s*$")
        permissions = re.search(r"(?ms)^    permissions:\n(?P<body>(?:^      [^\n]*\n)+)", active)
        self.assertIsNotNone(permissions, "acceptance job must declare its own permissions")
        self.assertEqual([line.strip() for line in permissions.group("body").splitlines()],
                         ["contents: read", "actions: read"])
        self.assertNotRegex(active, r"(?m)^    if:")
        self.assertNotRegex(active, r"(?m)^\s{6}(?:id-token|environment|secrets):")
        self.assertNotRegex(active, r"(?im)^\s*(?:continue-on-error|continue_on_error):")
        self.assertNotRegex(active, r"(?i)\b(?:secrets\.[A-Za-z0-9_]+|id-token:\s*write)\b")
        self.assertIn("persist-credentials: false", active)
        self.assertIn("--fresh-host", active)
        self.assertIn('trusted_runner="$GITHUB_WORKSPACE/infrastructure/deploy/tests/selfhost-acceptance.py"', active)
        self.assertIn('--bundle-identity "$RUNNER_TEMP/verified-selfhost/bundle-identity.json"', active)
        self.assertIn('python3 "$trusted_runner" "${args[@]}"', active)
        self.assertIn('cd "${roots[0]}"', active)
        self.assertIn("actions/upload-artifact", active)
        self.assertEqual(active.count("if: always()"), 1)
        upload_at = active.index("actions/upload-artifact")
        self.assertIn("if: always()", active[max(0, upload_at - 500):upload_at + 500])
        self.assertIn("if-no-files-found: error", active)
        self.assertIn("include-hidden-files: false", active)
        self.assertIn("retention-days: 14", active)
        self.assertEqual(active.count("evidence.json"), 1)
        self.assertRegex(active, r"(?m)^\s+path:\s*\$\{\{\s*runner\.temp\s*\}\}/selfhost-acceptance-evidence/evidence\.json\s*$")
        self.assertRegex(active, r"(?m)^\s+uses:\s*actions/checkout@[0-9a-f]{40}(?:\s+#.*)?$")
        self.assertRegex(active, r"(?m)^\s+uses:\s*actions/upload-artifact@[0-9a-f]{40}(?:\s+#.*)?$")
        evidence_step = active.index("- name: Create private evidence directory")
        prerequisite_step = active.index("- name: Install public host prerequisites")
        blocked_recorder = active.index("--blocked-prerequisite host-packages")
        fresh_runner = active.index("--fresh-host")
        self.assertLess(evidence_step, prerequisite_step,
                        "evidence destination must exist before a prerequisite can fail")
        self.assertLess(prerequisite_step, blocked_recorder)
        self.assertLess(blocked_recorder, fresh_runner)
        self.assertRegex(active,
                         r"(?m)^[ \t]+- name: Install public host prerequisites\n[ \t]+id: host-prerequisites[ \t]*$")
        self.assertRegex(active,
                         r"(?ms)- name: [^\n]*prerequisite[^\n]*failure[^\n]*\n\s+if: failure\(\) && steps\.host-prerequisites\.outcome == 'failure'\n(?:(?!- name:).)*?--blocked-prerequisite host-packages")
        prerequisite_block = active[prerequisite_step:blocked_recorder]
        # Package trust/startup refusal is exercised by the shared installer's
        # process-boundary tests; this consumer must invoke it before fixtures.
        install = 'bash "$GITHUB_WORKSPACE/scripts/ci/install-docker-engine.sh"'
        fixture = 'bash "$GITHUB_WORKSPACE/infrastructure/deploy/tests/selfhost-nginx-fixture.sh"'
        self.assertEqual(prerequisite_block.count(install), 1)
        self.assertLess(prerequisite_block.index(install), prerequisite_block.index(fixture))
        self.assertNotRegex(prerequisite_block,
                            r"(?m)^\s*curl\b[^\n]*\|\s*(?:sudo\s+)?(?:bash|sh)\b")
        for action in re.findall(r"(?m)^\s+uses:\s*(\S+)", active):
            self.assertRegex(action, r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$")
        self.assertNotRegex(active, r"(?m)^\s+(?:path|paths):\s*[^\n]*(?:\*|\.github/paths-filter)")
        self.assertNotIn("|| true", active)
        self.assertNotRegex(active, r"(?m)^\s*run:.*\bexit\s+0\b")
        self.assertNotRegex(source, r"(?ms)^  pull_request:\n(?:(?!^  [A-Za-z0-9_-]+:).)*?^\s+paths:")

    def test_routine_workflows_do_not_run_full_selfhost_acceptance(self):
        for workflow in ("pr-ci.yml", "main-ci.yml", "nightly-ci.yml", "main-cd.yml"):
            with self.subTest(workflow=workflow):
                source = (ROOT / ".github/workflows" / workflow).read_text(encoding="utf-8")
                self.assertIsNone(re.search(r"(?m)^  selfhost-acceptance:", source),
                                  "Routine CI must not define the full self-host acceptance job")
                self.assertFalse("infrastructure/deploy/tests/selfhost-acceptance.py" in source,
                                 "Routine CI must not invoke the full self-host acceptance runner")



class OverlappedStaticQueueTests(unittest.TestCase):
    """Regression #2506: the installer shares the first deployment queue wave."""

    INSTALLER = "scripts/tests/test-install-selfhost.sh"
    STORAGE = "infrastructure/deploy/tests/test-selfhost-storage.sh"
    # Deployment suites that run in their own pr-ci.yml job, not static-guards.
    OWN_JOB = {"infrastructure/deploy/tests/test-managed-firewall.sh": "managed-firewall"}

    def test_own_job_suites_run_in_their_own_job(self):
        import yaml
        jobs = yaml.safe_load(SelfhostPathFilterPolicyTests().source())["jobs"]
        for script, name in self.OWN_JOB.items():
            with self.subTest(script=script):
                self.assertTrue(any(step.get("run", "").strip() == "bash " + script
                                    for step in jobs[name]["steps"]),
                                f"{script} left static-guards but does not run in {name}")

    def test_managed_firewall_job_is_gated_on_its_own_filter(self):
        import yaml
        jobs = yaml.safe_load(SelfhostPathFilterPolicyTests().source())["jobs"]
        detect = jobs["detect-changes"]
        step = next(s for s in detect["steps"] if s.get("id") == "firewall-filter")
        self.assertEqual(detect["outputs"]["managed-firewall"],
                         "${{ steps.firewall-filter.outputs.managed_firewall }}")
        self.assertIn("needs.detect-changes.outputs.managed-firewall == 'true'", jobs["managed-firewall"]["if"])
        paths = yaml.safe_load(step["with"]["filters"])["managed_firewall"]
        for suite_input in ("infrastructure/docker/iptables/**",
                            "infrastructure/deploy/tests/managed_firewall_fixture.py",
                            "infrastructure/deploy/tests/managed_firewall_fake.py",
                            *self.OWN_JOB):
            self.assertIn(suite_input, paths)

    def invoke_owned_static_inventory(self, *, output="true", detector="success",
                                      ioc="success", cancelled=False, failure=None,
                                      failure_group="installer", rendezvous=False,
                                      observer_count=None):
        import shlex
        import yaml
        policy = SelfhostPathFilterPolicyTests()
        workflow = yaml.safe_load(policy.source())
        job = workflow["jobs"]["static-guards"]
        self.assertEqual(job["timeout-minutes"], 60, "scheduling must preserve the approved 3600-second job budget")
        git_env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        deployment = tuple(name for name in subprocess.check_output(
            ["git", "-C", str(ROOT), "ls-files", "--", "infrastructure/deploy/tests/test-*.sh"],
            env=git_env, text=True).splitlines() if name not in self.OWN_JOB)
        lifecycle = policy.LIFECYCLE_QUEUE_INVENTORY
        inventory = deployment + lifecycle
        self.assertGreater(len(deployment), 40, "owned inventory requires the actual retained deployment suites")
        self.assertEqual(len(inventory), len(set(inventory)), "owned inventory repeated a tracked consumer")
        self.assertIn(self.INSTALLER, inventory)
        self.assertIn(self.STORAGE, inventory)
        facts = dict(cancelled=cancelled, ioc=ioc, detector=detector, output=output)
        admitted = policy.decide(job["if"], **facts)
        relevant = detector != "success" or output != "false"
        expected = [name for name in inventory if admitted and
                    (relevant or name in deployment and not Path(name).name.startswith("test-selfhost-"))]
        steps = [step for step in job["steps"]
                 if "infrastructure/deploy/tests/test-*.sh" in step.get("run", "") or
                 any("bash " + name in step.get("run", "") for name in lifecycle)]
        self.assertTrue(steps, "actual workflow contains no deployment/lifecycle queue script")
        state = {"started": [], "completed": [], "active": [], "maximum": 0,
                 "paired": [], "unpaired": [], "pair_overlap": False,
                 "capacity_notified": [], "capacity_expired": []}
        results, waves = [], []
        with tempfile.TemporaryDirectory(prefix="owned-overlapped-static-queue-") as directory:
            root = Path(directory)
            control = root / "control"
            control.mkdir()
            (control / "state.json").write_text(json.dumps(state))
            failing = self.INSTALLER if failure_group == "installer" else deployment[0]
            (control / "config.json").write_text(json.dumps(
                {"inventory": inventory, "rendezvous": rendezvous,
                 "failing": failing if failure is not None else None, "failure": failure}))
            consumer = control / "consumer.py"
            consumer.write_text(policy.QUEUE_CONSUMER)
            for name in inventory:
                target = root / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text("#!/bin/bash\nexec " + shlex.quote(sys.executable) + " " +
                                  shlex.quote(str(consumer)) + ' "$PWD/' + name + '"\n')
            descriptors = []
            try:
                for phase in ("pair", "capacity"):
                    for number in (1, 2):
                        fifo = control / (phase + "-" + str(number))
                        os.mkfifo(fifo)
                        descriptors.append(os.open(fifo, os.O_RDWR | os.O_NONBLOCK))
                base_env = {"PATH": TEST_PATH, "LC_ALL": "C", "OWNED_QUEUE_ROOT": str(control)}
                if observer_count is not None:
                    names = (self.INSTALLER, self.STORAGE, deployment[0])[:observer_count]
                    processes = [subprocess.Popen([TEST_BASH, str(root / name)], cwd=root, env=base_env,
                                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                                 for name in names]
                    for process in processes:
                        stdout, stderr = process.communicate(timeout=15)
                        results.append(SimpleNamespace(returncode=process.returncode, stdout=stdout, stderr=stderr))
                elif admitted:
                    previous_success = True
                    for step in steps:
                        if not policy.decide(step.get("if", "True"), previous_success=previous_success, **facts):
                            continue
                        env = dict(base_env)
                        for key, value in step.get("env", {}).items():
                            env[key] = "true" if policy.decide(value, **facts) else "false"
                        before = json.loads((control / "state.json").read_text())["started"]
                        result = subprocess.run(
                            [TEST_BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step["run"]],
                            cwd=root, env=env, capture_output=True, text=True, timeout=20, check=False)
                        results.append(result)
                        after = json.loads((control / "state.json").read_text())["started"]
                        waves.append((step["name"], after[len(before):]))
                        previous_success = previous_success and result.returncode == 0
                state = json.loads((control / "state.json").read_text())
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)
        return results, state, expected, waves

    def assert_completed_inventory(self, results, state, expected, *, failed=False):
        self.assertEqual(any(result.returncode != 0 for result in results), failed,
                         "owned complete-script failure must decide the aggregate caller outcome")
        self.assertCountEqual(state["started"], expected,
                              "static queue abandoned or repeated a selected deployment/lifecycle consumer")
        self.assertCountEqual(state["completed"], expected,
                              "every selected deployment/lifecycle consumer must finish after child failure")
        self.assertEqual(state["active"], [], "static caller returned before owned script consumers completed")
        self.assertLessEqual(state["maximum"], 3, "static scheduling exceeded the three reviewed worker slots")

    def test_selected_installer_and_storage_enter_first_three_slots_in_one_common_queue(self):
        results, state, expected, waves = self.invoke_owned_static_inventory(rendezvous=True)
        self.assertTrue(results, "owned positive inventory did not execute the actual workflow scripts")
        self.assert_completed_inventory(results, state, expected)
        self.assertEqual((state["pair_overlap"], sorted(state["paired"]), state["unpaired"]),
                         (True, [1, 2], []), "first actual workflow pair did not reach both owned FIFO barriers")
        self.assertIn(self.INSTALLER, state["started"][:3],
                      "selected installer must occupy the first three shared worker slots")
        self.assertIn(self.STORAGE, state["started"][:3], "storage must retain its first-wave priority")
        self.assertEqual(len(waves), 1, "selected lifecycle and deployment consumers must share one bounded queue")
        self.assertCountEqual(waves[0][1], expected)

    def test_common_inventory_preserves_explicit_false_and_failed_or_unknown_detector_admission(self):
        for detector, output in (("success", "false"), ("success", ""), ("success", "unknown"),
                                 ("failure", "false"), ("skipped", "false"), ("cancelled", "false")):
            with self.subTest(detector=detector, output=output):
                results, state, expected, _ = self.invoke_owned_static_inventory(detector=detector, output=output)
                self.assertTrue(results, "unrelated deployment inventory must remain admitted")
                self.assert_completed_inventory(results, state, expected)
                if detector == "success" and output == "false":
                    self.assertNotIn(self.INSTALLER, state["started"], "explicit false reintroduced installer work")
                    self.assertNotIn(self.STORAGE, state["started"], "explicit false reintroduced self-host deployment work")
                else:
                    self.assertIn(self.INSTALLER, state["started"], "unknown or failed detection skipped installer work")
                    self.assertIn(self.STORAGE, state["started"], "unknown or failed detection skipped storage work")

    def test_common_inventory_finishes_every_consumer_after_failed_255_or_signalled_child(self):
        for group in ("installer", "deployment"):
            for failure in (1, 255, "signal"):
                with self.subTest(group=group, failure=failure):
                    results, state, expected, _ = self.invoke_owned_static_inventory(
                        failure=failure, failure_group=group)
                    self.assert_completed_inventory(results, state, expected, failed=True)

    def test_common_inventory_requires_successful_ioc_before_any_owned_consumer(self):
        for ioc, cancelled in (("failure", False), ("skipped", False), ("cancelled", False), ("success", True)):
            with self.subTest(ioc=ioc, cancelled=cancelled):
                results, state, expected, waves = self.invoke_owned_static_inventory(ioc=ioc, cancelled=cancelled)
                self.assertEqual(results, [])
                self.assertEqual(expected, [])
                self.assertEqual(waves, [])
                self.assert_completed_inventory(results, state, expected)

    def test_owned_installer_storage_pair_and_third_consumer_controls_observe_capacity(self):
        results, state, _, _ = self.invoke_owned_static_inventory(rendezvous=True, observer_count=2)
        self.assertTrue(all(result.returncode == 0 for result in results))
        self.assertCountEqual(state["started"], (self.INSTALLER, self.STORAGE))
        self.assertCountEqual(state["completed"], (self.INSTALLER, self.STORAGE))
        self.assertEqual((state["maximum"], state["pair_overlap"], sorted(state["paired"]), state["unpaired"]),
                         (2, True, [1, 2], []), "owned observer failed to see installer and storage simultaneously live")
        self.assertEqual(state["active"], [])
        results, state, _, _ = self.invoke_owned_static_inventory(rendezvous=True, observer_count=3)
        self.assertTrue(all(result.returncode == 0 for result in results))
        self.assertEqual(len(state["started"]), 3)
        self.assertEqual(len(state["completed"]), 3)
        self.assertEqual(state["maximum"], 3, "owned observer failed to detect an independently started third worker")
        self.assertEqual(sorted(state["capacity_notified"]), [1, 2])
        self.assertEqual(state["capacity_expired"], [])
        self.assertEqual(state["active"], [])
