#!/usr/bin/env python3
"""Construction coverage for the read-only self-host TLS observer.

The tests use synthetic Compose/inspect objects and benign loopback TLS only.
They never read or print a private-key file.  Runtime prerequisites are explicit:
Python 3.11/3.12 with OpenSSL partial-chain support and the `openssl` executable.
"""
from __future__ import annotations

import importlib.util
import builtins
import io
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[3]
MODULE_PATH = ROOT / "infrastructure/deploy/selfhost-tls.py"
CONFIG_SOURCE = "/fixture/infrastructure/docker/coturn/turnserver.conf"
CONFIG_TARGET = "/etc/coturn/turnserver.conf"


def load_subject():
    if not MODULE_PATH.is_file():
        raise RuntimeError("SETUP: selfhost-tls.py has not been constructed")
    spec = importlib.util.spec_from_file_location("selfhost_tls_subject", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError("SETUP: Python cannot load selfhost-tls.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


SUBJECT = load_subject()


def valid_model():
    return {
        "services": {
            "coturn": {
                "image": "coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e",
                # Production inherits the image's Config.User (nobody:nogroup);
                # no Compose `user` override is part of the ordinary model.
                "command": [
                    "turnserver", "-c", CONFIG_TARGET,
                    "--static-auth-secret=fixture-turn-secret",
                    "--realm=concordvoice.chat", "--listening-ip=0.0.0.0",
                    "--external-ip=192.0.2.10", "--relay-ip=192.0.2.10",
                    "--cert=/etc/coturn/certs/cert.pem",
                    "--pkey=/etc/coturn/certs/key.pem",
                ],
                "volumes": [{
                    "type": "bind", "source": "/opt/concord/certs/coturn",
                    "target": "/etc/coturn/certs", "read_only": True,
                }, {
                    "type": "bind", "source": CONFIG_SOURCE,
                    "target": CONFIG_TARGET, "read_only": True,
                }],
                # The shipped ordinary Compose model publishes 5349 on the
                # wildcard host address; production observer probes loopback.
                "ports": [{"target": 5349, "published": "5349", "protocol": "tcp"}],
            }
        }
    }


def valid_container():
    # A single standard Docker inspect record with only fields needed by this
    # contract. Config.Env and unrelated/raw inspect data are intentionally absent.
    return {
        "Config": {
            "Image": "coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e",
            "User": "nobody:nogroup",
            "Labels": {"com.docker.compose.project": "concordfixture", "com.docker.compose.service": "coturn"},
            "Cmd": list(valid_model()["services"]["coturn"]["command"]),
        },
        "State": {"Status": "running", "Running": True, "Paused": False, "Restarting": False},
        "Mounts": [
            {"Type": "bind", "Source": "/opt/concord/certs/coturn", "Destination": "/etc/coturn/certs", "RW": False},
            {"Type": "bind", "Source": CONFIG_SOURCE, "Destination": CONFIG_TARGET, "RW": False},
        ],
        "NetworkSettings": {"Ports": {"5349/tcp": [{"HostIp": "0.0.0.0", "HostPort": "5349"}]}},
    }


PROGRAMMING_ERRORS = (TypeError, KeyError, AttributeError, IndexError, AssertionError)
HANDSHAKE_REFUSAL_REASON = "self-host TLS handshake refused"
LEAF_MISMATCH_REASON = "self-host TLS leaf mismatch refused"
DEADLINE_REFUSAL_REASON = "self-host TLS deadline exceeded"
TLS_REFUSAL = getattr(SUBJECT, "TLSRefusalError", None)
if not isinstance(TLS_REFUSAL, type):
    raise RuntimeError("SETUP: selfhost-tls.py does not expose the fixed TLSRefusalError contract")


def assert_safe_refusal(test, callback, *args, expected_reason=None):
    """Require the fixed refusal class and a deterministic sanitized reason."""
    observed = []
    for _ in range(2):
        try:
            callback(*args)
        except Exception as exc:
            test.assertIsInstance(exc, TLS_REFUSAL,
                                  "failure did not use the fixed safe TLS refusal class")
            test.assertNotIsInstance(exc, PROGRAMMING_ERRORS,
                                     "failure came from an accidental programming exception")
            message = str(exc)
            test.assertTrue(message, "refusal must carry a fixed safe reason")
            if expected_reason is not None:
                test.assertEqual(message, expected_reason, "refusal used the wrong fixed stage reason")
            lowered = message.lower()
            for forbidden in ("private-key sentinel", "fixture-turn-secret", "-----begin", "opaque-stderr",
                              "traceback", "/opt/concord/", "/tmp/alternate", ".pem"):
                test.assertNotIn(forbidden, lowered, "refusal exposed fixture material or raw diagnostics")
            observed.append((type(exc), message))
        else:
            test.fail("invalid input was admitted")
    test.assertEqual(observed[0], observed[1], "same refusal did not return a stable safe reason")


def assert_one_safe_refusal(test, callback, *args, expected_reason=None):
    try:
        callback(*args)
    except Exception as exc:
        test.assertIsInstance(exc, TLS_REFUSAL,
                              "failure did not use the fixed safe TLS refusal class")
        test.assertNotIsInstance(exc, PROGRAMMING_ERRORS,
                                 "failure came from an accidental programming exception")
        message = str(exc)
        test.assertTrue(message, "refusal must carry a fixed safe reason")
        if expected_reason is not None:
            test.assertEqual(message, expected_reason, "refusal used the wrong fixed stage reason")
        lowered = message.lower()
        for forbidden in ("private-key sentinel", "fixture-turn-secret", "-----begin", "opaque-stderr",
                          "traceback", "/opt/concord/", "/tmp/alternate", ".pem"):
            test.assertNotIn(forbidden, lowered, "refusal exposed fixture material or raw diagnostics")
    else:
        test.fail("invalid input was admitted")


def assert_no_key_reads(test, key_path, callback, *args):
    """Observe ordinary Python file-opening paths without reading fixture key bytes."""
    key_path = os.path.realpath(key_path)
    original_builtin, original_io = builtins.open, io.open
    original_path_open = Path.open
    original_os_open = os.open
    reads = []

    def is_key(file):
        try: return os.path.realpath(os.fsdecode(os.fspath(file))) == key_path
        except (TypeError, ValueError, OSError): return False

    def track(original):
        def wrapped(file, *open_args, **kwargs):
            if is_key(file): reads.append(True)
            return original(file, *open_args, **kwargs)
        return wrapped

    with patch.object(builtins, "open", track(original_builtin)), \
         patch.object(io, "open", track(original_io)), \
         patch.object(Path, "open", track(original_path_open)), \
         patch.object(os, "open", track(original_os_open)):
        callback(*args)
    test.assertFalse(reads, "observer/admission opened a private-key path")


class TestStructuredAdmission(unittest.TestCase):
    def test_model_accepts_canonical_read_only_coturn_consumer(self):
        SUBJECT.validate_model(valid_model())

    def test_model_rejects_mutated_consumer_contract(self):
        mutations = (
            lambda m: m["services"]["coturn"].update(image="coturn/coturn:latest"),
            lambda m: m["services"]["coturn"]["volumes"][0].update(read_only=False),
            lambda m: m["services"]["coturn"]["volumes"][0].update(source="/tmp/alternate"),
            lambda m: m["services"]["coturn"]["volumes"][0].update(target="/wrong"),
            lambda m: (m["services"]["coturn"]["command"].__setitem__(2, "/wrong/turnserver.conf"),
                       m["services"]["coturn"]["command"].append(CONFIG_TARGET)),
            lambda m: m["services"]["coturn"]["volumes"].pop(1),
            lambda m: m["services"]["coturn"]["volumes"][1].update(read_only=False),
            lambda m: m["services"]["coturn"]["volumes"][1].update(source="infrastructure/docker/coturn/turnserver.conf"),
            lambda m: m["services"]["coturn"]["volumes"].append({
                "type": "bind", "source": "/tmp/alternate", "target": "/etc/coturn/certs/key.pem",
                "read_only": True,
            }),
            lambda m: m["services"]["coturn"]["volumes"].append({
                "type": "bind", "source": "/tmp/alternate", "target": "/etc/coturn/certs/cert.pem",
                "read_only": True,
            }),
            lambda m: m["services"]["coturn"].update(command=["--cert", "/wrong/cert.pem"]),
            lambda m: m["services"]["coturn"]["command"].append("--cli"),
            lambda m: m["services"]["coturn"]["command"].append("--cli=true"),
            lambda m: m["services"]["coturn"].update(ports=[]),
            lambda m: m["services"]["coturn"]["ports"][0].update(protocol="udp"),
            lambda m: m["services"]["coturn"]["ports"][0].update(published="5348"),
        )
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                model = valid_model()
                mutate(model)
                assert_safe_refusal(self, SUBJECT.validate_model, model)

    def test_container_accepts_exact_running_project_mount_and_listener(self):
        SUBJECT.validate_container(valid_container(), "concordfixture")

    def test_container_rejects_wrong_project_state_identity_mount_or_publication(self):
        changes = (
            lambda c: c["Config"]["Labels"].update({"com.docker.compose.project": "other"}),
            lambda c: c["Config"]["Labels"].update({"com.docker.compose.service": "other"}),
            lambda c: c["State"].update(Status="exited", Running=False),
            lambda c: c["State"].update(Paused=True),
            lambda c: c["State"].update(Restarting=True),
            lambda c: c["State"].update(Status="unknown", Running=False, Paused=False, Restarting=False),
            lambda c: c["State"].update(Status="exited", Running=False, Paused=True, Restarting=False),
            lambda c: c["Mounts"][0].update(RW=True),
            lambda c: c["Mounts"][0].update(Source="/tmp/alternate"),
            lambda c: (c["Config"]["Cmd"].__setitem__(2, "/wrong/turnserver.conf"),
                       c["Config"]["Cmd"].append(CONFIG_TARGET)),
            lambda c: c["Mounts"].pop(1),
            lambda c: c["Mounts"][1].update(RW=True),
            lambda c: c["Mounts"][1].update(Source="infrastructure/docker/coturn/turnserver.conf"),
            lambda c: c["Mounts"].append({
                "Type": "bind", "Source": "/tmp/alternate", "Destination": "/etc/coturn/certs/key.pem", "RW": False,
            }),
            lambda c: c["Mounts"].append({
                "Type": "bind", "Source": "/tmp/alternate", "Destination": "/etc/coturn/certs/cert.pem", "RW": False,
            }),
            lambda c: c["Config"].update(User="root"),
            lambda c: c["Config"].update(Cmd=["--cert", "/wrong/cert.pem"]),
            lambda c: c["Config"]["Cmd"].append("--cli"),
            lambda c: c["Config"]["Cmd"].append("--cli=true"),
            lambda c: c["NetworkSettings"]["Ports"].update({"5349/tcp": []}),
        )
        for mutate in changes:
            with self.subTest(mutation=mutate):
                container = valid_container()
                mutate(container)
                assert_safe_refusal(self, SUBJECT.validate_container, container, "concordfixture")

    def test_selected_projection_rejects_cli_activation(self):
        path = ROOT / "infrastructure/deploy/tests/test-selfhost-tls-selected-consumer.py"
        spec = importlib.util.spec_from_file_location("selected_consumer", path)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        selected = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(selected)
        model = valid_model()
        model["services"]["coturn"]["container_name"] = selected.CONTAINER_NAME
        selected.validate_projection(model, ROOT)
        for flag in ("--cli", "--cli=true"):
            with self.subTest(flag=flag):
                mutated = valid_model()
                service = mutated["services"]["coturn"]
                service["container_name"] = selected.CONTAINER_NAME
                service["command"].append(flag)
                with self.assertRaises(selected.AcceptanceFailure):
                    selected.validate_projection(mutated, ROOT)


class TestMetadataAdmission(unittest.TestCase):
    def test_exact_root_and_coturn_uid_metadata_is_admitted(self):
        if os.geteuid() != 0:
            self.fail("SETUP: run this required metadata test in the own-root public fixture")
        with tempfile.TemporaryDirectory(prefix="concord-tls-metadata-") as temp:
            root = Path(temp) / "certs"
            coturn = root / "coturn"
            coturn.mkdir(parents=True)
            os.chown(root, 0, 0)
            root.chmod(0o711)
            os.chown(coturn, 0, 0)
            coturn.chmod(0o711)
            cert, key = coturn / "cert.pem", coturn / "key.pem"
            cert.write_text("public certificate fixture\n")
            key.write_text("private-key sentinel fixture\n")
            os.chown(cert, 0, 0); os.chmod(cert, 0o444)
            os.chown(key, 65534, 65533); os.chmod(key, 0o400)
            assert_no_key_reads(self, key, SUBJECT.validate_metadata, str(coturn))

    def test_metadata_rejects_wrong_type_owner_or_mode(self):
        if os.geteuid() != 0:
            self.fail("SETUP: run this required metadata test in the own-root public fixture")
        with tempfile.TemporaryDirectory(prefix="concord-tls-metadata-") as temp:
            base = Path(temp)
            for case in ("directory", "link", "missing-cert", "missing-key", "empty-cert", "empty-key",
                         "owner", "mode", "key-mode", "key-owner", "child-mode", "parent-mode"):
                with self.subTest(case=case):
                    root = base / case / "certs"
                    coturn = root / "coturn"
                    coturn.mkdir(parents=True)
                    os.chown(root, 0, 0)
                    root.chmod(0o711)
                    os.chown(coturn, 0, 0)
                    coturn.chmod(0o711)
                    cert = coturn / "cert.pem"
                    key = coturn / "key.pem"
                    cert.write_text("public certificate fixture\n")
                    key.write_text("private-key sentinel fixture\n")
                    os.chown(cert, 0, 0); os.chmod(cert, 0o444)
                    os.chown(key, 65534, 65533); os.chmod(key, 0o400)
                    if case == "directory":
                        cert.unlink(); cert.mkdir()
                    elif case == "link":
                        cert.unlink(); cert.symlink_to(key)
                    elif case == "missing-cert": cert.unlink()
                    elif case == "missing-key": key.unlink()
                    elif case == "empty-cert": cert.write_text("")
                    elif case == "empty-key": key.write_text("")
                    elif case == "owner": os.chown(cert, 65534, 65533)
                    elif case == "mode": os.chmod(cert, 0o444 | 0o020)
                    elif case == "key-mode": os.chmod(key, 0o440)
                    elif case == "key-owner": os.chown(key, 0, 0)
                    elif case == "child-mode":
                        coturn.chmod(0o751)
                    elif case == "parent-mode":
                        root.chmod(0o755)
                    assert_safe_refusal(self, SUBJECT.validate_metadata, str(coturn))


class LoopbackTLS(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if shutil.which("openssl") is None:
            raise RuntimeError("SETUP: openssl executable is required for benign loopback fixtures")
        if not hasattr(ssl, "VERIFY_X509_PARTIAL_CHAIN"):
            raise RuntimeError("SETUP: Python/OpenSSL lacks VERIFY_X509_PARTIAL_CHAIN")
        cls.temp = tempfile.TemporaryDirectory(prefix="concord-tls-loopback-")
        cls.path = Path(cls.temp.name)
        cls.domain = "example.invalid"
        cls.ca_key, cls.ca_cert = cls.path / "ca.key", cls.path / "ca.pem"
        cls.leaf_key, cls.leaf_cert = cls.path / "leaf.key", cls.path / "leaf.pem"
        cls.other_key, cls.other_cert = cls.path / "other.key", cls.path / "other.pem"
        cls.self_key, cls.self_cert = cls.path / "self.key", cls.path / "self.pem"
        cls._openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3", "-subj", "/CN=fixture CA", "-keyout", str(cls.ca_key), "-out", str(cls.ca_cert), "-addext", "basicConstraints=critical,CA:TRUE")
        cls._make_leaf(cls.leaf_key, cls.leaf_cert, "DNS:example.invalid")
        cls.fullchain = cls.path / "fullchain.pem"
        cls.fullchain.write_bytes(cls.leaf_cert.read_bytes() + cls.ca_cert.read_bytes())
        cls._make_leaf(cls.other_key, cls.other_cert, "DNS:example.invalid")
        cls._openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3", "-sha256", "-subj", "/CN=ignored.invalid", "-addext", "subjectAltName=DNS:example.invalid", "-keyout", str(cls.self_key), "-out", str(cls.self_cert))
        cls.cn_key, cls.cn_cert = cls.path / "cn-only.key", cls.path / "cn-only.pem"
        cls._openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3", "-sha256", "-subj", "/CN=example.invalid", "-keyout", str(cls.cn_key), "-out", str(cls.cn_cert))
        cls.wrong_key, cls.wrong_cert = cls.path / "wrong.key", cls.path / "wrong.pem"
        cls._make_leaf(cls.wrong_key, cls.wrong_cert, "DNS:wrong.invalid")

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    @classmethod
    def _openssl(cls, *args):
        subprocess.run(["openssl", *args], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)

    @classmethod
    def _make_leaf(cls, key, cert, san):
        csr = cert.with_suffix(".csr")
        ext = cert.with_suffix(".ext")
        ext.write_text("basicConstraints=critical,CA:FALSE\nsubjectAltName=" + san + "\n")
        cls._openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=ignored.invalid", "-keyout", str(key), "-out", str(csr))
        cls._openssl("x509", "-req", "-in", str(csr), "-CA", str(cls.ca_cert), "-CAkey", str(cls.ca_key), "-CAcreateserial", "-days", "3", "-sha256", "-extfile", str(ext), "-out", str(cert))

    def start_server(self, cert, key, behavior="tls"):
        ready = threading.Event()
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind(("127.0.0.1", 0)); listener.listen(4); listener.settimeout(0.2)
        port = listener.getsockname()[1]
        stop = threading.Event()

        def serve():
            ready.set()
            while not stop.is_set():
                try: conn, _ = listener.accept()
                except socket.timeout: continue
                except OSError: return
                if behavior == "eof":
                    conn.close(); continue
                if behavior == "stall":
                    stop.wait(5); conn.close(); continue
                try:
                    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
                    context.load_cert_chain(str(cert), str(key))
                    tls_conn = context.wrap_socket(conn, server_side=True)
                    tls_conn.close()
                except (OSError, ssl.SSLError):
                    try: conn.close()
                    except OSError: pass

        thread = threading.Thread(target=serve, daemon=True)
        thread.start(); ready.wait(1)
        return listener, stop, thread, port

    def test_fullchain_uses_only_first_non_ca_leaf_as_anchor_and_ignores_ambient_roots(self):
        # Inspect the trust-loading inputs and default-store APIs directly.
        # OpenSSL's CA enumeration does not reliably list a non-CA trust anchor.
        old = os.environ.pop("SSLKEYLOGFILE", None)
        old_file = os.environ.get("SSL_CERT_FILE")
        old_dir = os.environ.get("SSL_CERT_DIR")
        ambient_dir = self.path / "ambient-ca-dir"
        ambient_dir.mkdir()
        os.environ["SSL_CERT_FILE"] = str(self.ca_cert)
        os.environ["SSL_CERT_DIR"] = str(ambient_dir)
        expected_leaf = ssl.PEM_cert_to_DER_cert(self.leaf_cert.read_text())
        original_load = ssl.SSLContext.load_verify_locations
        original_load_defaults = ssl.SSLContext.load_default_certs
        original_set_default_paths = ssl.SSLContext.set_default_verify_paths
        trust_load_calls = []
        default_store_calls = []

        def capture_trust_load(context, *args, **kwargs):
            before_stats = context.cert_store_stats()
            result = original_load(context, *args, **kwargs)
            trust_load_calls.append((args, kwargs, before_stats))
            return result

        def capture_load_defaults(context, *args, **kwargs):
            default_store_calls.append("load_default_certs")
            return original_load_defaults(context, *args, **kwargs)

        def capture_set_default_paths(context, *args, **kwargs):
            default_store_calls.append("set_default_verify_paths")
            return original_set_default_paths(context, *args, **kwargs)

        try:
            listener, stop, thread, port = self.start_server(self.leaf_cert, self.leaf_key)
            try:
                with patch.object(ssl.SSLContext, "load_verify_locations", capture_trust_load), \
                     patch.object(ssl.SSLContext, "load_default_certs", capture_load_defaults), \
                     patch.object(ssl.SSLContext, "set_default_verify_paths", capture_set_default_paths):
                    assert_no_key_reads(self, self.leaf_key,
                                        SUBJECT.observe_tls, str(self.fullchain), self.domain, port, 5)
                self.assertEqual(len(trust_load_calls), 1,
                                 "observer did not load exactly one selected certificate anchor")
                args, kwargs, before_stats = trust_load_calls[0]
                self.assertEqual(args, (), "selected anchor was not passed as explicit cadata")
                self.assertEqual(set(kwargs), {"cadata"},
                                 "observer passed a cafile/capath or extra trust input")
                cadata = kwargs["cadata"]
                self.assertIsInstance(cadata, str)
                self.assertEqual(cadata.count("-----BEGIN CERTIFICATE-----"), 1,
                                 "observer passed more than the first fullchain certificate")
                self.assertEqual(ssl.PEM_cert_to_DER_cert(cadata), expected_leaf,
                                 "loaded trust input was not the selected first leaf")
                self.assertEqual(before_stats["x509_ca"], 0,
                                 "ambient/default CA roots entered the context before the leaf load")
                self.assertEqual(default_store_calls, [],
                                 "observer invoked an ambient default trust-store loader")
            finally:
                stop.set(); listener.close(); thread.join(timeout=1)
        finally:
            if old is not None: os.environ["SSLKEYLOGFILE"] = old
            if old_file is None: os.environ.pop("SSL_CERT_FILE", None)
            else: os.environ["SSL_CERT_FILE"] = old_file
            if old_dir is None: os.environ.pop("SSL_CERT_DIR", None)
            else: os.environ["SSL_CERT_DIR"] = old_dir

    def test_matching_common_name_without_san_is_rejected(self):
        listener, stop, thread, port = self.start_server(self.cn_cert, self.cn_key)
        try:
            assert_one_safe_refusal(self, SUBJECT.observe_tls,
                                    str(self.cn_cert), self.domain, port, 3,
                                    expected_reason=HANDSHAKE_REFUSAL_REASON)
        finally:
            stop.set(); listener.close(); thread.join(timeout=1)

    def test_self_signed_apex_leaf_is_supported(self):
        listener, stop, thread, port = self.start_server(self.self_cert, self.self_key)
        try:
            SUBJECT.observe_tls(str(self.self_cert), self.domain, port, 5)
        finally:
            stop.set(); listener.close(); thread.join(timeout=1)

    def test_attempt_timeout_is_clipped_to_three_seconds_and_caller_budget(self):
        # Instrument only public stdlib seams. The TLS exchange remains real and
        # loopback; fake monotonic ticks keep this a deterministic deadline test.
        listener, stop, thread, port = self.start_server(self.leaf_cert, self.leaf_key)
        start = 40.0
        clock = [start]
        observed_timeouts = []
        original_socket_settimeout = socket.socket.settimeout
        original_ssl_settimeout = ssl.SSLSocket.settimeout
        original_connect = socket.socket.connect
        original_handshake = ssl.SSLSocket.do_handshake
        connect_timeouts = []
        handshake_timeouts = []

        def fake_monotonic():
            clock[0] += 0.001
            return clock[0]

        def track_socket_timeout(sock, value):
            observed_timeouts.append(value)
            return original_socket_settimeout(sock, value)

        def track_ssl_timeout(sock, value):
            observed_timeouts.append(value)
            return original_ssl_settimeout(sock, value)

        def track_connect(sock, address):
            connect_timeouts.append(sock.gettimeout())
            result = original_connect(sock, address)
            # Charge deterministic elapsed time between connection and TLS.
            clock[0] += 0.18
            return result

        def track_handshake(sock, *args, **kwargs):
            if not sock.server_side:
                handshake_timeouts.append(sock.gettimeout())
            return original_handshake(sock, *args, **kwargs)

        try:
            with patch.object(time, "monotonic", fake_monotonic), \
                 patch.object(socket.socket, "settimeout", track_socket_timeout), \
                 patch.object(ssl.SSLSocket, "settimeout", track_ssl_timeout), \
                 patch.object(socket.socket, "connect", track_connect), \
                 patch.object(ssl.SSLSocket, "do_handshake", track_handshake):
                assert_no_key_reads(self, self.leaf_key,
                                    SUBJECT.observe_tls, str(self.leaf_cert), self.domain, port, 0.4)
                clipped = [value for value in observed_timeouts if isinstance(value, (int, float)) and value > 0]
                self.assertTrue(clipped, "observer did not bound a socket operation")
                self.assertTrue(all(value <= 0.4 for value in clipped), "socket timeout ignored caller remainder")
                self.assertTrue(connect_timeouts and handshake_timeouts,
                                "instrumentation did not observe both connect and TLS handshake")
                self.assertLess(handshake_timeouts[0], connect_timeouts[0] - 0.1,
                                "handshake received a fresh per-attempt budget instead of the same remaining deadline")
                self.assertLessEqual(clock[0] - start, 0.4, "observer reset or exceeded the caller monotonic budget")
                observed_timeouts.clear()
                before_full_attempt = clock[0]
                assert_no_key_reads(self, self.leaf_key,
                                    SUBJECT.observe_tls, str(self.leaf_cert), self.domain, port, 10)
                capped = [value for value in observed_timeouts if isinstance(value, (int, float)) and value > 0]
                self.assertTrue(capped, "observer did not set an attempt timeout")
                self.assertTrue(all(value <= 3.0 for value in capped), "an individual socket operation exceeded 3 seconds")
                self.assertLessEqual(clock[0] - before_full_attempt, 10,
                                     "observer reset or exceeded its shared monotonic budget")
        finally:
            stop.set(); listener.close(); thread.join(timeout=1)

    def test_refused_connections_use_deadline_clipped_retry_delay(self):
        # Refusal is immediate and wholly synthetic: neither connect nor sleep
        # touches the network or wall clock. Advancing the fake clock on every
        # read also makes a missing-delay implementation terminate.
        start = 80.0
        clock = [start]
        budget = 0.12
        deadline = [None]
        attempts = []
        delays = []

        class RefusedSocket:
            def settimeout(self, _value):
                pass

            def connect(self, _address):
                attempts.append(True)
                raise ConnectionRefusedError("synthetic refusal")

            def close(self):
                pass

        def fake_monotonic():
            clock[0] += 0.001
            if deadline[0] is None:
                deadline[0] = clock[0] + budget
            return clock[0]

        def fake_sleep(delay):
            remaining = deadline[0] - clock[0]
            delays.append((delay, remaining))
            clock[0] += max(0.0, delay)

        with patch.object(time, "monotonic", fake_monotonic), \
             patch.object(time, "sleep", fake_sleep), \
             patch.object(socket, "socket", side_effect=lambda *_args, **_kwargs: RefusedSocket()):
            assert_one_safe_refusal(self, SUBJECT.observe_tls, str(self.leaf_cert),
                                    self.domain, 5349, budget,
                                    expected_reason=HANDSHAKE_REFUSAL_REASON)

        self.assertTrue(attempts, "refusal fixture did not reach connect")
        self.assertTrue(delays, "immediate refused connections were retried without pacing")
        self.assertTrue(all(delay > 0 for delay, _remaining in delays),
                        "retry delay must be positive")
        self.assertTrue(all(delay <= remaining for delay, remaining in delays),
                        "retry sleep extended past the shared deadline")
        self.assertLess(len(attempts), 20,
                        "connection refusal retry loop was not bounded by backoff")

    def test_wrong_name_different_leaf_eof_and_stall_do_not_admit(self):
        for cert, key, behavior, domain, reason in (
            (self.leaf_cert, self.leaf_key, "tls", "wrong.invalid", HANDSHAKE_REFUSAL_REASON),
            (self.other_cert, self.other_key, "tls", self.domain, HANDSHAKE_REFUSAL_REASON),
            (self.leaf_cert, self.leaf_key, "eof", self.domain, HANDSHAKE_REFUSAL_REASON),
            (self.leaf_cert, self.leaf_key, "stall", self.domain, DEADLINE_REFUSAL_REASON),
        ):
            with self.subTest(behavior=behavior, domain=domain, certificate=cert.name):
                listener, stop, thread, port = self.start_server(cert, key, behavior)
                began = time.monotonic()
                try:
                    assert_one_safe_refusal(self, SUBJECT.observe_tls, str(self.leaf_cert), domain, port,
                                            0.25 if behavior == "stall" else 4,
                                            expected_reason=reason)
                finally:
                    stop.set(); listener.close(); thread.join(timeout=1)
                self.assertLess(time.monotonic() - began, 1.5 if behavior == "stall" else 6,
                                "observer did not clip the connection/handshake to the caller remainder")

    def test_completed_handshake_still_requires_exact_peer_der_equality(self):
        listener, stop, thread, port = self.start_server(self.leaf_cert, self.leaf_key)
        original_handshake = ssl.SSLSocket.do_handshake
        completed = []

        def tracked_handshake(sock, *args, **kwargs):
            result = original_handshake(sock, *args, **kwargs)
            completed.append(True)
            return result

        def different_peer_der(sock, *args, **kwargs):
            self.assertTrue(completed, "DER equality branch was reached before TLS completed")
            return b"different-public-leaf"

        try:
            with patch.object(ssl.SSLSocket, "do_handshake", tracked_handshake), \
                 patch.object(ssl.SSLSocket, "getpeercert", different_peer_der):
                assert_safe_refusal(self, SUBJECT.observe_tls, str(self.leaf_cert), self.domain, port, 5,
                                    expected_reason=LEAF_MISMATCH_REASON)
        finally:
            stop.set(); listener.close(); thread.join(timeout=1)

    def test_selected_public_leaf_replacement_during_handshake_is_refused(self):
        original_public_leaf = self.leaf_cert.read_bytes()
        listener, stop, thread, port = self.start_server(self.leaf_cert, self.leaf_key)
        original_getpeercert = ssl.SSLSocket.getpeercert
        original_handshake = ssl.SSLSocket.do_handshake
        replaced = []
        completed = []

        def tracked_handshake(sock, *args, **kwargs):
            result = original_handshake(sock, *args, **kwargs)
            completed.append(True)
            return result

        def replace_after_completed_handshake(sock, *args, **kwargs):
            self.assertTrue(completed, "leaf replacement happened before TLS completed")
            peer = original_getpeercert(sock, *args, **kwargs)
            if kwargs.get("binary_form") and not replaced:
                self.leaf_cert.write_bytes(self.other_cert.read_bytes())
                replaced.append(True)
            return peer

        try:
            with patch.object(ssl.SSLSocket, "do_handshake", tracked_handshake), \
                 patch.object(ssl.SSLSocket, "getpeercert", replace_after_completed_handshake):
                assert_one_safe_refusal(self, SUBJECT.observe_tls,
                                        str(self.leaf_cert), self.domain, port, 5,
                                        expected_reason=LEAF_MISMATCH_REASON)
            self.assertTrue(replaced, "fixture did not replace the selected public leaf after handshake")
        finally:
            self.leaf_cert.write_bytes(original_public_leaf)
            stop.set(); listener.close(); thread.join(timeout=1)

    def test_tcp_only_listener_is_not_tls_success(self):
        ready = threading.Event()
        stop = threading.Event()
        listener = socket.socket(); listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind(("127.0.0.1", 0)); listener.listen(4); listener.settimeout(0.2)
        port = listener.getsockname()[1]

        def accept_and_close():
            ready.set()
            while not stop.is_set():
                try:
                    connection, _ = listener.accept()
                except socket.timeout:
                    continue
                except OSError:
                    return
                connection.close()

        thread = threading.Thread(target=accept_and_close, daemon=True)
        thread.start(); ready.wait(1)
        try:
            assert_one_safe_refusal(self, SUBJECT.observe_tls, str(self.leaf_cert), self.domain, port, 3,
                                    expected_reason=HANDSHAKE_REFUSAL_REASON)
        finally:
            stop.set(); listener.close(); thread.join(timeout=1)

    def test_ambient_sslkeylogfile_is_not_created(self):
        old = os.environ.get("SSLKEYLOGFILE")
        keylog = self.path / "ambient-keylog.txt"
        os.environ["SSLKEYLOGFILE"] = str(keylog)
        listener, stop, thread, port = self.start_server(self.leaf_cert, self.leaf_key)
        try:
            assert_no_key_reads(self, self.leaf_key,
                                SUBJECT.observe_tls, str(self.leaf_cert), self.domain, port, 5)
            self.assertFalse(keylog.exists(), "observer honored ambient key logging")
        finally:
            if old is None: os.environ.pop("SSLKEYLOGFILE", None)
            else: os.environ["SSLKEYLOGFILE"] = old
            stop.set(); listener.close(); thread.join(timeout=1)


if __name__ == "__main__":
    unittest.main(verbosity=2)
