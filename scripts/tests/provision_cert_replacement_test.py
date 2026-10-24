"""Owned certificate-generation replacement tests; no privileged host effects."""
import fcntl
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import shutil
import stat
import ssl
import subprocess
import sys
import tempfile
import unittest

SOURCE_ROOT = Path(sys.argv.pop(1)).resolve()
BASE = "replacement.example"
REAL_OPENSSL = shutil.which("openssl")
REAL_PYTHON = sys.executable

# Real file operations, virtual ownership, and closed Docker/systemd boundaries.
# Observe canonical bytes after each filesystem effect: replacing cert and key
# with two separate renames is visible here, even if the final pair matches.
HOST = r'''
import json, os, pathlib, shutil, signal, stat, subprocess, sys
name, args = pathlib.Path(sys.argv[0]).stem, sys.argv[1:]
root = pathlib.Path(os.environ['PAIR_ROOT'])
meta_path = root/'metadata.json'
meta = json.loads(meta_path.read_text())
fault = os.environ.get('PAIR_FAULT', '')
def owned(value):
    path = pathlib.Path(value)
    if not path.is_absolute(): path = pathlib.Path.cwd()/path
    assert root == path or root in path.parents, 'non-owned path refused'
    if path.exists(): assert root == path.resolve() or root in path.resolve().parents
    return path
def event(value):
    with (root/'effects').open('a') as out: out.write(value+'\n')
def observe():
    live = pathlib.Path(os.environ['LE_LIVE_ROOT'])/os.environ['PAIR_BASE']
    cert, key = live/'fullchain.pem', live/'privkey.pem'
    if cert.exists() and key.exists():
        c = subprocess.run([os.environ['REAL_OPENSSL'], 'x509', '-in', str(cert), '-noout', '-pubkey'], capture_output=True)
        k = subprocess.run([os.environ['REAL_OPENSSL'], 'pkey', '-in', str(key), '-pubout'], capture_output=True)
        if c.returncode or k.returncode or not c.stdout or c.stdout != k.stdout:
            event('consumer-observed-mixed-pair')
    elif cert.exists() or key.exists(): event('consumer-observed-incomplete-pair')
def inject(label):
    if (root/'fault-fired').exists() and fault != 'delivery-key-persistent-fail': return
    (root/'fault-fired').write_text(label)
    event('injected-'+label)
    if fault.endswith('interrupt'):
        os.kill(int(os.environ['PAIR_MAIN_PID']), signal.SIGTERM)
    sys.exit(74)
def save(): meta_path.write_text(json.dumps(meta))
if name == 'openssl':
    if args and args[0] == 'req':
        assert args[args.index('-newkey')+1] == 'rsa:4096'
        key = owned(args[args.index('-keyout')+1]); cert = owned(args[args.index('-out')+1])
        shutil.copyfile(root/'next'/'privkey.pem', key); key.chmod(0o600)
        event('local-key-write'); observe()
        if fault.startswith('local-second-'): inject(fault)
        shutil.copyfile(root/'next'/'fullchain.pem', cert)
        event('local-cert-write'); observe()
    else: os.execv(os.environ['REAL_OPENSSL'], [os.environ['REAL_OPENSSL']]+args)
elif name == 'stat':
    if args[:2] in (['-Lc','%u:%g:%a'], ['-c','%u:%g:%a']):
        path = owned(args[2]); mode = stat.S_IMODE(path.stat().st_mode)
        uid, gid = meta.get(str(path), [0,0,mode])[:2]
        print(f'{uid}:{gid}:{mode:o}')
    else: os.execv('/usr/bin/stat', ['/usr/bin/stat']+args)
elif name == 'install':
    mode = int(args[args.index('-m')+1],8)
    uid = int(args[args.index('-o')+1]) if args[args.index('-o')+1] != 'root' else 0
    gid = int(args[args.index('-g')+1]) if args[args.index('-g')+1] != 'root' else 0
    dest = owned(args[-1])
    if '-d' in args: dest.mkdir(parents=True, exist_ok=True)
    else:
        src = owned(args[-2])
        if fault.startswith('delivery-key-') and dest == root/'fanout'/'coturn'/'key.pem': inject(fault)
        # GNU install replaces an existing read-only destination as root; use
        # an owned unlink/copy here rather than granting actual host privilege.
        if dest.is_file(): dest.unlink()
        renewal_install = dest == root/'hooks/copy-certs.sh' or (
            dest.name == 'installed' and dest.parent.parent == root/'hooks' and
            dest.parent.name.startswith('.copy-certs.'))
        if fault == 'renewal-hook-partial-copy-fail' and renewal_install:
            payload = src.read_bytes()
            assert len(payload) > 64, 'complete owned renewal candidate required'
            dest.write_bytes(payload[:64]); dest.chmod(mode)
            meta[str(dest)] = [uid,gid,mode]; save()
            event('renewal-hook-partial-write')
            inject(fault)
        shutil.copyfile(src,dest)
        if dest == root/'fanout'/'coturn'/'cert.pem': event('delivery-certificate-write')
        if dest == root/'fanout'/'coturn'/'key.pem': event('delivery-key-write')
    dest.chmod(mode); meta[str(dest)] = [uid,gid,mode]; save(); observe()
elif name == 'cp':
    src, dest = owned(args[-2]), owned(args[-1])
    if dest.is_dir(): dest = dest/src.name
    if fault.startswith('import-second-') and dest.name == 'privkey.pem': inject(fault)
    if fault in ('import-staged-san-missing','import-staged-future') and src.parent == root/'next':
        assert dest.parent.parent == pathlib.Path(os.environ['LE_LIVE_ROOT'])
        assert dest.parent != pathlib.Path(os.environ['LE_LIVE_ROOT'])/os.environ['PAIR_BASE']
        src = owned(root/'copied-input'/src.name)
        (root/'fault-fired').write_text(fault)
        event('injected-staged-'+dest.name)
    shutil.copyfile(src,dest); event('copy-'+dest.name); observe()
elif name == 'mv':
    src, dest = owned(args[-2]), owned(args[-1])
    if dest.is_dir() and '-T' not in args: dest = dest/src.name
    if dest == root/'hooks/copy-certs.sh':
        assert src.name == 'installed' and src.parent.parent == root/'hooks'
        assert src.parent.name.startswith('.copy-certs.')
        event('renewal-hook-activation-attempt')
        if fault == 'renewal-hook-pre-rename-fail': inject(fault)
    os.replace(src,dest)
    for key in list(meta):
        if key == str(src) or key.startswith(str(src)+'/'):
            meta[str(dest)+key[len(str(src)):]] = meta.pop(key)
    save(); event('rename'); observe()
elif name == 'ln':
    assert any('s' in arg for arg in args[:-2]), 'only symbolic fixture links'
    target, dest = args[-2], owned(args[-1])
    resolved = pathlib.Path(target) if pathlib.Path(target).is_absolute() else dest.parent/target
    owned(str(resolved))
    if dest.is_symlink() and any('f' in arg for arg in args[:-2]): dest.unlink()
    os.symlink(target,dest); event('link'); observe()
elif name == 'chown':
    identity = args[0]; uid,gid = identity.split(':')
    uid = 0 if uid == 'root' else int(uid); gid = 0 if gid == 'root' else int(gid)
    for value in args[1:]:
        path = owned(value); meta[str(path)] = [uid,gid,stat.S_IMODE(path.stat().st_mode)]
    save()
elif name == 'chmod':
    mode = int(args[0],8)
    for value in args[1:]:
        path = owned(value); path.chmod(mode)
        meta.setdefault(str(path),[0,0,mode])[2] = mode
    save(); observe()
elif name == 'rm':
    for value in [arg for arg in args if not arg.startswith('-')]:
        path = owned(value)
        if path.is_symlink() or path.is_file(): path.unlink()
        elif path.is_dir(): shutil.rmtree(path)
elif name == 'docker':
    running = os.environ.get('PAIR_RUNNING_CONSUMERS') == '1'
    ident = '123456789abc'
    if args and args[0] == 'ps' and '--all' in args:
        event('coturn-discovery')
        if running:
            print(json.dumps({'ID':ident,'Names':'concordvoice-coturn','Labels':{
                'com.docker.compose.project':'owned','com.docker.compose.service':'coturn'}}))
    elif running and args[:2] == ['inspect','--format'] and args[-1] == ident:
        event('coturn-inspection')
        print(json.dumps({'Name':'/concordvoice-coturn','Image':'coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e',
            'Project':'owned','Service':'coturn','Status':'running','Running':True,
            'Paused':False,'Restarting':False}))
    elif running and args == ['restart',ident]: event('coturn-restart')
    else: raise RuntimeError('unexpected closed Docker operation')
elif name == 'systemctl':
    running = os.environ.get('PAIR_RUNNING_CONSUMERS') == '1'
    if args == ['is-active','--quiet','nginx']:
        event('nginx-active-check'); sys.exit(0 if running else 3)
    elif running and args == ['reload','nginx']: event('nginx-reload')
    else: raise RuntimeError('unexpected closed systemd operation')
elif name == 'certbot':
    assert args[0] == 'certonly' and '--reuse-key' in args and '--no-directory-hooks' in args
    event('certbot-managed-lineage')
    if fault == 'certbot-issuance-fail':
        event('certbot-issuance-refused')
        sys.exit(1)
    if fault == 'certbot-no-lineage': event('certbot-lineage-not-created')
else: raise RuntimeError('unsupported owned command')
'''

# Ordinary syscall fault injection at the atomic namespace boundary. All path
# arguments remain constrained to this fixture's owned directory descriptors.
# The successful syscall remains real, including a TERM immediately afterwards.
ATOMIC_RUNNER = r'''
import ctypes, errno, json, os, pathlib, signal, sys
args = sys.argv[1:]
if len(args) < 2 or args[0] != '-c':
    os.execv(os.environ['REAL_PYTHON'],[os.environ['REAL_PYTHON']]+args)
root = pathlib.Path(os.environ['PAIR_ROOT'])
fault = os.environ.get('PAIR_FAULT','')
original_cdll = ctypes.CDLL
def library(*values,**options):
    result = original_cdll(*values,**options)
    actual = getattr(result,'renameat2',None)
    if actual is None: return result
    def rename(source_fd,source,target_fd,target,flags):
        for fd,name in ((source_fd,source),(target_fd,target)):
            parent = pathlib.Path(os.readlink('/proc/self/fd/'+str(fd)))
            name = os.fsdecode(name)
            assert root == parent or root in parent.parents
            assert '/' not in name and name not in ('','.','..')
        first = not (root/'fault-fired').exists()
        if first and fault in ('activation-unsupported','activation-fail'):
            (root/'fault-fired').write_text(fault)
            ctypes.set_errno(errno.ENOSYS if fault == 'activation-unsupported' else errno.EIO)
            return -1
        actual.argtypes = rename.argtypes; actual.restype = rename.restype
        status = actual(source_fd,source,target_fd,target,flags)
        if status == 0:
            with (root/'effects').open('a') as out: out.write('atomic-generation-activation\n')
        if status == 0 and first and fault in ('activation-interrupt','activation-interrupt-int'):
            (root/'fault-fired').write_text(fault)
            os.kill(int(os.environ['PAIR_MAIN_PID']),signal.SIGINT if fault.endswith('-int') else signal.SIGTERM)
        return status
    result.renameat2 = rename
    return result
ctypes.CDLL = library
sys.argv = ['-c']+args[2:]
exec(compile(args[1],'<owned-atomic-generation>','exec'),{'__name__':'__main__'})
'''


def fixture_command(args):
    result = subprocess.run(args, capture_output=True, timeout=30)
    if result.returncode:
        raise AssertionError("owned OpenSSL fixture failed")
    return result.stdout


class CertificateReplacement(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.pairs = tempfile.TemporaryDirectory(prefix="concord-owned-cert-inputs-")
        cls.inputs = Path(cls.pairs.name)
        for label, base in (("previous", BASE), ("next", BASE), ("unrelated", "unrelated.example")):
            path = cls.inputs/label
            path.mkdir()
            fixture_command([REAL_OPENSSL, "req", "-x509", "-newkey", "ec",
                             "-pkeyopt", "ec_paramgen_curve:P-384", "-sha256", "-nodes",
                             "-days", "30", "-subj", "/CN="+base, "-addext",
                             "subjectAltName="+",".join("DNS:"+h for h in
                                 (base, "api."+base, "media."+base, "turn."+base, "spa."+base)),
                             "-keyout", str(path/"privkey.pem"), "-out", str(path/"fullchain.pem")])
            (path/"privkey.pem").chmod(0o600)
        # Regression PR #3584: checkend only checks notAfter. OpenSSL signs a
        # matching SAN-covered pair with an independently controlled notBefore.
        future = cls.inputs/"future"
        future.mkdir(mode=0o700)
        shutil.copyfile(cls.inputs/"next/privkey.pem", future/"privkey.pem")
        (future/"privkey.pem").chmod(0o600)
        fixture_command([REAL_OPENSSL,"req","-new","-key",str(future/"privkey.pem"),
                         "-subj","/CN="+BASE,"-out",str(future/"request.csr")])
        (future/"index").write_text("")
        (future/"serial").write_text("1000\n")
        config = future/"ca.cnf"
        config.write_text("[ca]\ndefault_ca=owned\n[owned]\n"
                          "database="+str(future/"index")+"\nserial="+str(future/"serial")+
                          "\nnew_certs_dir="+str(future)+"\ndefault_md=sha256\n"
                          "policy=subject\n[subject]\ncommonName=supplied\n"
                          "[leaf]\nsubjectAltName="+",".join("DNS:"+host for host in
                              (BASE,"api."+BASE,"media."+BASE,"turn."+BASE,"spa."+BASE))+"\n")
        now = datetime.now(timezone.utc)
        fixture_command([REAL_OPENSSL,"ca","-selfsign","-batch","-notext","-config",str(config),
                         "-keyfile",str(future/"privkey.pem"),"-in",str(future/"request.csr"),
                         "-extensions","leaf","-startdate",(now+timedelta(days=2)).strftime("%Y%m%d%H%M%SZ"),
                         "-enddate",(now+timedelta(days=32)).strftime("%Y%m%d%H%M%SZ"),
                         "-out",str(future/"fullchain.pem")])

    @classmethod
    def tearDownClass(cls):
        cls.pairs.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="concord-cert-replacement-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for label in ("source", "bin", "live", "hooks", "next"):
            (self.root/label).mkdir()
        for name in ("provision-cert.sh", "copy-certs.sh"):
            source = (SOURCE_ROOT/"infrastructure/deploy"/name).read_text()
            (self.root/"source"/name).write_text(source.replace('"$EUID"', '"${PAIR_EUID:-0}"'))
        for name in ("openssl", "stat", "install", "cp", "mv", "ln", "chown", "chmod", "rm", "docker", "systemctl", "certbot"):
            driver = self.root/"bin"/(name+".py")
            driver.write_text(HOST)
            wrapper = self.root/"bin"/name
            wrapper.write_text('#!/bin/bash\nexec "$REAL_PYTHON" "'+str(driver)+'" "$@"\n')
            wrapper.chmod(0o755)
        atomic_driver = self.root/"atomic-runner.py"
        atomic_driver.write_text(ATOMIC_RUNNER)
        wrapper = self.root/"bin/python3"
        wrapper.write_text('#!/bin/bash\nexec "$REAL_PYTHON" "'+str(atomic_driver)+'" "$@"\n')
        wrapper.chmod(0o755)
        self.live = self.root/"live"/BASE
        shutil.copytree(self.inputs/"previous", self.live)
        for name in ("fullchain.pem", "privkey.pem"):
            shutil.copyfile(self.inputs/"next"/name, self.root/"next"/name)
        (self.root/"next"/"privkey.pem").chmod(0o600)
        (self.live/"fullchain.pem").chmod(0o644)
        self.fanout = self.root/"fanout"/"coturn"
        self.fanout.mkdir(parents=True)
        self.fanout.parent.chmod(0o711); self.fanout.chmod(0o711)
        shutil.copyfile(self.live/"fullchain.pem", self.fanout/"cert.pem")
        shutil.copyfile(self.live/"privkey.pem", self.fanout/"key.pem")
        (self.fanout/"cert.pem").chmod(0o444); (self.fanout/"key.pem").chmod(0o400)
        (self.root/"metadata.json").write_text(json.dumps({str(self.fanout/"key.pem"): [65534,65533,0o400]}))
        (self.root/"effects").write_text("")
        self.env = dict(os.environ, PAIR_ROOT=str(self.root), PAIR_BASE=BASE,
                        REAL_OPENSSL=REAL_OPENSSL, REAL_PYTHON=REAL_PYTHON,
                        LE_LIVE_ROOT=str(self.root/"live"), HOOK_DIR=str(self.root/"hooks"),
                        COTURN_CERT_ROOT=str(self.fanout.parent), PATH=str(self.root/"bin")+":"+os.environ["PATH"])

    def invoke(self, mode="import", fault="", *, euid=0, domain=BASE):
        args = ["--host", BASE]
        if mode == "import": args += ["--cert", str(self.root/"next"/"fullchain.pem"), "--key", str(self.root/"next"/"privkey.pem")]
        elif mode == "local": args += ["--days", "30"]
        elif mode == "letsencrypt": args = ["--domain", domain, "--email", "fixture@example.invalid"]
        result = subprocess.run(["bash", "-c", 'export PAIR_MAIN_PID=$BASHPID; exec bash "$@"', "owned-cert-main",
                                 str(self.root/"source"/"provision-cert.sh"), mode, *args],
                                env=dict(self.env, PAIR_FAULT=fault, PAIR_EUID=str(euid)), capture_output=True, timeout=30)
        output = (result.stdout+result.stderr).decode(errors="replace")
        for error in ("Traceback", "command not found", "unbound variable", "syntax error"):
            self.assertNotIn(error, output, "fixture must reach production replacement boundary")
        self.assertNotIn("PRIVATE KEY", output, "replacement must not emit key material")
        return result.returncode, output

    def assert_previous_pair(self):
        self.assertTrue((self.live/"fullchain.pem").read_bytes() == (self.inputs/"previous"/"fullchain.pem").read_bytes(),
                        "previous complete canonical certificate must survive failed replacement")
        self.assertTrue((self.live/"privkey.pem").read_bytes() == (self.inputs/"previous"/"privkey.pem").read_bytes(),
                        "previous complete canonical key must survive failed replacement")

    def assert_validity_fixture(self, directory, *, future):
        cert,key = directory/"fullchain.pem",directory/"privkey.pem"
        public = fixture_command([REAL_OPENSSL,"x509","-in",str(cert),"-noout","-pubkey"])
        self.assertTrue(public and public == fixture_command([REAL_OPENSSL,"pkey","-in",str(key),"-pubout"]),
                        "validity fixture must be a structurally valid matching pair")
        fixture_command([REAL_OPENSSL,"x509","-in",str(cert),"-noout","-checkend","0"])
        for host in (BASE,"api."+BASE,"media."+BASE,"turn."+BASE,"spa."+BASE):
            checked = fixture_command([REAL_OPENSSL,"x509","-in",str(cert),"-noout","-checkhost",host])
            self.assertEqual("Hostname "+host+" does match certificate",checked.decode().strip())
        metadata = ssl._ssl._test_decode_cert(str(cert))
        now = datetime.now(timezone.utc).timestamp()
        starts,ends = (ssl.cert_time_to_seconds(metadata[name]) for name in ("notBefore","notAfter"))
        self.assertGreater(ends,now,"future validity fixture must not be expired")
        if future:
            self.assertGreater(starts,now+86400,"only notBefore must prohibit current admission")
        else:
            self.assertLessEqual(starts,now,"positive control must already be valid")
        verified = subprocess.run([REAL_OPENSSL,"verify","-CAfile",str(cert),str(cert)],capture_output=True,timeout=30)
        if future:
            self.assertNotEqual(0,verified.returncode)
            self.assertIn("certificate is not yet valid",verified.stderr.decode(),
                          "independent OpenSSL chain verification must confirm the exact temporal defect")
        else: self.assertEqual(0,verified.returncode,verified.stderr.decode())

    def validity_snapshot(self):
        paths = [self.live/"fullchain.pem",self.live/"privkey.pem",self.fanout/"cert.pem",
                 self.fanout/"key.pem",self.root/"next/fullchain.pem",self.root/"next/privkey.pem"]
        return [(path.read_bytes(),path.stat().st_ino,stat.S_IMODE(path.stat().st_mode),
                 path.stat().st_mtime_ns) for path in paths]

    def use_future_pair(self, directory):
        directory.mkdir(exist_ok=True,mode=0o700)
        for name in ("fullchain.pem","privkey.pem"):
            shutil.copyfile(self.inputs/"future"/name,directory/name)
        (directory/"fullchain.pem").chmod(0o644)
        (directory/"privkey.pem").chmod(0o600)
        self.assert_validity_fixture(directory,future=True)

    def assert_temporal_refusal(self, rc, output, before, *, staged=False, delivery=False):
        effects = (self.root/"effects").read_text()
        if staged:
            self.assertTrue((self.root/"fault-fired").is_file(),"future pair must replace the copied candidate")
            self.assertIn("injected-staged-fullchain.pem",effects)
            self.assertIn("injected-staged-privkey.pem",effects)
        self.assertEqual(1,rc,"future notBefore must refuse before active generation replacement, delivery, or service restart; "
                         "actual exit="+str(rc)+"; effects="+repr(effects)+"; output="+output)
        self.assertNotRegex(output,"(?i)SAN does not cover|SPKI mismatch|transaction lock unavailable",
                            "temporal refusal must not be a SAN, key, or locking refusal")
        self.assertRegex(output.lower(),"not yet valid|validity|source (invalid|incomplete)|staging failed",
                         "temporal refusal must report the invalid certificate boundary")
        self.assertTrue(before == self.validity_snapshot(),"temporal refusal must preserve source, active and delivery bytes and metadata")
        for forbidden in ("atomic-generation-activation","delivery-certificate-write","delivery-key-write",
                          "coturn-discovery","coturn-restart","nginx-active-check","nginx-reload"):
            self.assertNotIn(forbidden,effects,"future notBefore must precede "+forbidden)
        self.assertNotIn("Certificate source and coturn delivery complete.",output)
        self.assertNotIn("copy-certs: certificate pair delivery complete.",output)
        if not delivery:
            self.assertEqual([],list((self.root/"live").glob(".*.generation.*")),"refused candidate must be cleaned")

    def invoke_delivery(self):
        result = subprocess.run(["bash",str(self.root/"source/copy-certs.sh")],
                                env=dict(self.env,RENEWED_LINEAGE=str(self.live),PAIR_EUID="0"),
                                capture_output=True,timeout=30)
        output = (result.stdout+result.stderr).decode(errors="replace")
        for error in ("Traceback","command not found","unbound variable","syntax error","PRIVATE KEY"):
            self.assertNotIn(error,output,"owned delivery must reach actual certificate validation")
        return result.returncode,output

    def test_future_not_before_import_refuses_before_activation_delivery_and_restart(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.use_future_pair(self.root/"next")
        before = self.validity_snapshot()
        rc,output = self.invoke()
        self.assert_temporal_refusal(rc,output,before)

    def test_future_not_before_copied_pair_refuses_before_activation_delivery_and_restart(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.assert_validity_fixture(self.root/"next",future=False)
        self.use_future_pair(self.root/"copied-input")
        before = self.validity_snapshot()
        rc,output = self.invoke(fault="import-staged-future")
        self.assert_temporal_refusal(rc,output,before,staged=True)

    def test_future_not_before_delivery_refuses_before_copy_and_restart(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.use_future_pair(self.live)
        before = self.validity_snapshot()
        rc,output = self.invoke_delivery()
        self.assert_temporal_refusal(rc,output,before,delivery=True)

    def test_future_not_before_source_delivery_refuses_before_copy_and_restart(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.use_future_pair(self.live)
        self.certbot_lineage()
        before = self.validity_snapshot()
        rc,output = self.invoke("letsencrypt")
        self.assertIn("certbot-managed-lineage",(self.root/"effects").read_text(),
                      "actual main must reach complete_delivery for the Certbot-managed source")
        self.assert_temporal_refusal(rc,output,before,delivery=True)

    def assert_running_delivery(self):
        effects = (self.root/"effects").read_text().splitlines()
        for name in ("delivery-certificate-write","delivery-key-write","coturn-restart","nginx-reload"):
            self.assertEqual(1,effects.count(name),"already-valid positive control must reach "+name)
        self.assertLess(effects.index("delivery-key-write"),effects.index("coturn-restart"))
        self.assertLess(effects.index("coturn-restart"),effects.index("nginx-reload"))
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (self.live/"fullchain.pem").read_bytes())
        self.assertTrue((self.fanout/"key.pem").read_bytes() == (self.live/"privkey.pem").read_bytes())

    def test_already_valid_import_activates_delivers_and_restarts(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.assert_validity_fixture(self.root/"next",future=False)
        self.assert_success("import")
        self.assertEqual(1,(self.root/"effects").read_text().splitlines().count("atomic-generation-activation"))
        self.assert_running_delivery()

    def test_already_valid_delivery_copies_and_restarts(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.assert_validity_fixture(self.live,future=False)
        rc,output = self.invoke_delivery()
        self.assertEqual(0,rc,output)
        self.assertIn("copy-certs: coturn-stage=restarted",output)
        self.assert_running_delivery()

    def test_already_valid_certbot_lineage_delivers_and_restarts(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.assert_validity_fixture(self.live,future=False)
        self.certbot_lineage()
        links = [os.readlink(self.live/name) for name in ("fullchain.pem","privkey.pem")]
        rc,output = self.invoke("letsencrypt")
        self.assertEqual(0,rc,output)
        self.assertIn("Certificate source and coturn delivery complete.",output)
        self.assertEqual(links,[os.readlink(self.live/name) for name in ("fullchain.pem","privkey.pem")])
        self.assertNotIn("atomic-generation-activation",(self.root/"effects").read_text())
        self.assert_running_delivery()

    def provision_installed_renewal_hook(self):
        self.env["PAIR_RUNNING_CONSUMERS"] = "1"
        self.assert_validity_fixture(self.live, future=False)
        self.certbot_lineage()
        rc, output = self.invoke("letsencrypt")
        self.assertEqual(0, rc, output)
        self.assert_running_delivery()
        # Installation shape may change to bind a lineage. Exercise that
        # installed executable, rather than requiring a verbatim source copy.
        hook = self.root/"hooks/copy-certs.sh"
        self.assertTrue(hook.is_file() and os.access(hook, os.X_OK),
                        "actual provisioning must install the renewal executable")
        (self.root/"effects").write_text("")
        return hook

    def invoke_installed_renewal(self, hook, lineage, domains):
        result = subprocess.run(["bash", str(hook)],
                                env=dict(self.env, RENEWED_LINEAGE=str(lineage),
                                         RENEWED_DOMAINS=domains, PAIR_EUID="0"),
                                capture_output=True, timeout=30)
        output = (result.stdout+result.stderr).decode(errors="replace")
        for error in ("Traceback", "command not found", "unbound variable", "syntax error", "PRIVATE KEY"):
            self.assertNotIn(error, output, "actual installed hook must reach the renewal lineage boundary")
        return result.returncode, output

    def renewal_snapshot(self, *roots):
        records = []
        for root in roots:
            for path in (root, *sorted(root.rglob("*"))):
                info = path.lstat()
                records.append((str(path), info.st_ino, info.st_mode, info.st_uid, info.st_gid,
                                info.st_size, info.st_mtime_ns,
                                os.readlink(path) if path.is_symlink() else None,
                                path.read_bytes() if path.is_file() else None))
        return records

    def assert_unrelated_installed_renewal_has_no_effects(self, lineage, pair):
        # Regression PR #3584: all directory hooks run for every Certbot cert.
        hook = self.provision_installed_renewal_hook()
        shutil.copytree(pair, lineage)
        cert, key = lineage/"fullchain.pem", lineage/"privkey.pem"
        (lineage/"privkey.pem").chmod(0o600)
        self.assertTrue(fixture_command([REAL_OPENSSL, "x509", "-in", str(cert), "-noout", "-pubkey"]) ==
                        fixture_command([REAL_OPENSSL, "pkey", "-in", str(key), "-pubout"]),
                        "unrelated lineage fixture must have a real matching key")
        fixture_command([REAL_OPENSSL, "verify", "-CAfile", str(cert), str(cert)])
        before = self.renewal_snapshot(self.fanout.parent, self.root/"live", lineage,
                                       self.root/"hooks", self.root/"metadata.json")
        rc, output = self.invoke_installed_renewal(hook, lineage, BASE+" api."+BASE)
        self.assertIn(rc, (0, 1), "unrelated lineage must be ignored or deliberately refused")
        with self.subTest(boundary="fan-out and services"):
            self.assertEqual("", (self.root/"effects").read_text(),
                             "unrelated Certbot lineage must be ignored before fan-out, Coturn discovery/restart, or nginx reload")
        with self.subTest(boundary="certificate bytes and metadata"):
            self.assertTrue(before == self.renewal_snapshot(self.fanout.parent, self.root/"live", lineage,
                                                           self.root/"hooks", self.root/"metadata.json"),
                            "unrelated Certbot lineage must preserve certificate bytes, ownership, modes, and metadata")

    def test_installed_renewal_hook_ignores_unrelated_lineage_before_effects(self):
        self.assert_unrelated_installed_renewal_has_no_effects(self.root/"live/unrelated.example",
                                                            self.inputs/"unrelated")

    def test_installed_renewal_hook_ignores_prefix_sibling_lineage_before_effects(self):
        self.assert_unrelated_installed_renewal_has_no_effects(self.root/"live"/(BASE+"-other"),
                                                            self.inputs/"unrelated")

    def test_installed_renewal_hook_ignores_same_name_in_different_root_before_effects(self):
        self.assert_unrelated_installed_renewal_has_no_effects(self.root/"alternate-live"/BASE,
                                                            self.inputs/"next")

    def assert_selected_installed_renewal_delivers(self, domains):
        hook = self.provision_installed_renewal_hook()
        self.assertFalse((self.inputs/"previous/fullchain.pem").read_bytes() ==
                         (self.inputs/"next/fullchain.pem").read_bytes(),
                         "renewal positive control must change the previously delivered pair")
        # Simulate Certbot's successful archive rotation through its live links;
        # neither the hook source nor the installed executable is changed.
        for name in ("fullchain.pem", "privkey.pem"):
            shutil.copyfile(self.inputs/"next"/name, self.live/name)
        self.assert_validity_fixture(self.live, future=False)
        rc, output = self.invoke_installed_renewal(hook, self.live, domains)
        self.assertEqual(0, rc, output)
        self.assert_running_delivery()

    def test_installed_renewal_hook_delivers_selected_lineage(self):
        self.assert_selected_installed_renewal_delivers(BASE+" api."+BASE+" media."+BASE+" turn."+BASE)

    def test_installed_renewal_hook_delivers_selected_lineage_with_reordered_domains(self):
        self.assert_selected_installed_renewal_delivers("turn."+BASE+" media."+BASE+" api."+BASE+" "+BASE)

    def next_renewal_helper(self):
        # Owned candidate differs from the working installed generation, but
        # keeps all real delivery behavior. No production source is changed.
        helper = self.root/"source/copy-certs.sh"
        helper.write_bytes(helper.read_bytes()+b"\n# owned next renewal-helper generation\n")
        return helper.read_bytes()

    def installed_hook_snapshot(self, hook):
        metadata = json.loads((self.root/"metadata.json").read_text())
        return self.renewal_snapshot(hook), metadata.get(str(hook))

    def hook_contract_snapshot(self, hook):
        metadata = json.loads((self.root/"metadata.json").read_text())
        return hook.read_bytes(), stat.S_IMODE(hook.stat().st_mode), metadata.get(str(hook))

    def test_partial_renewal_hook_install_preserves_working_target_before_certbot(self):
        # Regression PR #3584: failed preparation cannot destroy the timer's
        # previously working executable before Certbot starts.
        hook = self.provision_installed_renewal_hook()
        rc, output = self.invoke_installed_renewal(hook, self.live, BASE)
        self.assertEqual(0, rc, output)
        self.assert_running_delivery()
        (self.root/"effects").write_text("")
        self.next_renewal_helper()
        before_hook, before_pairs = self.installed_hook_snapshot(hook), self.validity_snapshot()
        rc, output = self.invoke("letsencrypt", fault="renewal-hook-partial-copy-fail")
        self.assertEqual("renewal-hook-partial-copy-fail", (self.root/"fault-fired").read_text(),
                         "finite partial-copy fault must be consumed at actual renewal installation")
        effects = (self.root/"effects").read_text()
        self.assertIn("renewal-hook-partial-write", effects,
                      "actual candidate installation must perform the owned partial write before failing")
        self.assertEqual(1, rc, "partial hook preparation must refuse issuance")
        self.assertIn("renewal hook installation failed; delivery not complete", output)
        self.assertNotIn("certbot-managed-lineage", effects,
                         "failed renewal hook preparation must stop before Certbot")
        for forbidden in ("delivery-certificate-write", "delivery-key-write", "coturn-discovery",
                          "coturn-restart", "nginx-reload"):
            self.assertNotIn(forbidden, effects, "failed hook preparation must precede "+forbidden)
        self.assertTrue(before_pairs == self.validity_snapshot(),
                        "failed hook preparation must preserve active and delivered certificate pairs")
        self.assertTrue(before_hook == self.installed_hook_snapshot(hook),
                        "failed renewal hook preparation must preserve the previously working target bytes and metadata")

    def test_completed_renewal_hook_activation_delivers_complete_selected_helper(self):
        hook = self.provision_installed_renewal_hook()
        previous = hook.read_bytes()
        desired_helper = self.next_renewal_helper()
        rc, output = self.invoke("letsencrypt")
        self.assertEqual(0, rc, output)
        self.assertTrue(hook.read_bytes() != previous and hook.read_bytes().endswith(desired_helper),
                        "completed renewal activation must install the complete new helper, not retain stale or partial bytes")
        self.assertEqual(0o755, stat.S_IMODE(hook.stat().st_mode))
        self.assertEqual([0, 0, 0o755], json.loads((self.root/"metadata.json").read_text())[str(hook)],
                         "completed renewal activation must retain root ownership and executable permissions")
        self.assertEqual(1, (self.root/"effects").read_text().splitlines().count("certbot-managed-lineage"))
        (self.root/"effects").write_text("")
        for name in ("fullchain.pem", "privkey.pem"):
            shutil.copyfile(self.inputs/"next"/name, self.live/name)
        rc, output = self.invoke_installed_renewal(hook, self.live, "turn."+BASE+" "+BASE)
        self.assertEqual(0, rc, output)
        self.assert_running_delivery()
        unrelated = self.root/"live/unrelated.example"
        shutil.copytree(self.inputs/"unrelated", unrelated)
        (unrelated/"privkey.pem").chmod(0o600)
        (self.root/"effects").write_text("")
        before = self.renewal_snapshot(self.fanout.parent)
        rc, output = self.invoke_installed_renewal(hook, unrelated, BASE)
        self.assertEqual(0, rc, output)
        self.assertEqual("", (self.root/"effects").read_text(),
                         "completed helper activation must still bind the selected lineage before service effects")
        self.assertTrue(before == self.renewal_snapshot(self.fanout.parent))

    def test_failed_renewal_hook_rename_preserves_working_target_before_certbot(self):
        hook = self.provision_installed_renewal_hook()
        rc, output = self.invoke_installed_renewal(hook, self.live, BASE)
        self.assertEqual(0, rc, output)
        self.assert_running_delivery()
        (self.root/"effects").write_text("")
        self.next_renewal_helper()
        before_hook, before_pairs = self.installed_hook_snapshot(hook), self.validity_snapshot()
        rc, output = self.invoke("letsencrypt", fault="renewal-hook-pre-rename-fail")
        self.assertEqual("renewal-hook-pre-rename-fail", (self.root/"fault-fired").read_text(),
                         "finite rename failure must be consumed before replacing the actual working hook")
        effects = (self.root/"effects").read_text()
        self.assertIn("renewal-hook-activation-attempt", effects,
                      "prepared bytes and permissions must reach actual hook activation")
        self.assertNotIn("rename", effects.splitlines(),
                         "pre-rename failure must not perform namespace activation")
        self.assertEqual(1, rc, "failed hook activation must refuse issuance")
        self.assertIn("renewal hook installation failed; delivery not complete", output)
        self.assertNotIn("certbot-managed-lineage", effects,
                         "failed renewal hook activation must stop before Certbot")
        for forbidden in ("delivery-certificate-write", "delivery-key-write", "coturn-discovery",
                          "coturn-restart", "nginx-reload"):
            self.assertNotIn(forbidden, effects, "failed hook activation must precede "+forbidden)
        self.assertTrue(before_pairs == self.validity_snapshot(),
                        "failed hook activation must preserve active and delivered certificate pairs")
        self.assertTrue(before_hook == self.installed_hook_snapshot(hook),
                        "failed renewal hook activation must preserve the previously working target bytes and metadata")

    def assert_failed_letsencrypt_preserves_previous_hook(self, fault):
        hook = self.provision_installed_renewal_hook()
        previous = self.hook_contract_snapshot(hook)
        self.next_renewal_helper()
        (self.root/"effects").write_text("")
        rc, output = self.invoke("letsencrypt", fault=fault)
        self.assertNotEqual(0, rc, "failed issuance or delivery must refuse completion")
        effects = (self.root/"effects").read_text()
        if fault == "certbot-issuance-fail":
            self.assertIn("certbot-issuance-refused", effects,
                          "issuance failure control must be consumed after hook preparation")
            self.assertNotIn("delivery-certificate-write", effects)
        else:
            self.assertIn("certbot-managed-lineage", effects,
                          "delivery failure control must occur after successful issuance")
            self.assertIn("injected-delivery-key-fail", effects)
        self.assertTrue(previous == self.hook_contract_snapshot(hook),
                        "failed new issuance or certificate delivery preserves the previous renewal hook bytes, mode and lineage")

    def test_failed_new_certificate_issuance_preserves_previous_renewal_hook(self):
        self.assert_failed_letsencrypt_preserves_previous_hook("certbot-issuance-fail")

    def test_failed_new_certificate_delivery_preserves_previous_renewal_hook(self):
        self.assert_failed_letsencrypt_preserves_previous_hook("delivery-key-fail")

    def assert_domain_change_failure_preserves_old_lineage(self, fault):
        hook = self.provision_installed_renewal_hook()
        previous = self.hook_contract_snapshot(hook)
        next_domain = "replacement-next.example"
        (self.root/"effects").write_text("")
        rc, output = self.invoke("letsencrypt", fault=fault, domain=next_domain)
        self.assertNotEqual(0, rc, "failed new-domain issuance or source validation must refuse completion")
        effects = (self.root/"effects").read_text()
        if fault == "certbot-issuance-fail":
            self.assertIn("certbot-issuance-refused", effects,
                          "domain-change issuance failure must occur after installing the attempted new binding")
        else:
            self.assertIn("certbot-lineage-not-created", effects,
                          "domain-change incomplete-source failure must follow successful Certbot invocation")
            self.assertNotIn("delivery-certificate-write", effects)
        self.assertTrue(previous == self.hook_contract_snapshot(hook),
                        "failed new-domain issuance or certificate delivery preserves the previous renewal hook bytes, mode and lineage")

    def test_new_domain_issuance_failure_preserves_old_renewal_lineage(self):
        self.assert_domain_change_failure_preserves_old_lineage("certbot-issuance-fail")

    def test_new_domain_incomplete_source_preserves_old_renewal_lineage(self):
        self.assert_domain_change_failure_preserves_old_lineage("certbot-no-lineage")

    def test_failed_first_issuance_does_not_leave_new_renewal_hook(self):
        # No existing installation: a failed first issuance must not leave a
        # renewal consumer configured for a lineage that was never delivered.
        rc, output = self.invoke("letsencrypt", fault="certbot-issuance-fail")
        self.assertNotEqual(0, rc, "failed first issuance must refuse completion")
        self.assertIn("certbot-issuance-refused", (self.root/"effects").read_text(),
                      "first-issuance fault must reach Certbot after hook preparation")
        hook = self.root/"hooks/copy-certs.sh"
        self.assertFalse(hook.exists(),
                         "failed first issuance must preserve the absent prior renewal hook")

    def assert_success(self, mode):
        rc, output = self.invoke(mode)
        self.assertEqual(0, rc, output)
        self.assertIn("Certificate source and coturn delivery complete.", output)
        self.assertNotIn("consumer-observed-mixed-pair", (self.root/"effects").read_text(),
                         "canonical consumer must never see cert/key from different generations")
        self.assertNotIn("consumer-observed-incomplete-pair", (self.root/"effects").read_text())
        self.assertTrue((self.live/"fullchain.pem").read_bytes() == (self.root/"next"/"fullchain.pem").read_bytes())
        self.assertTrue((self.live/"privkey.pem").read_bytes() == (self.root/"next"/"privkey.pem").read_bytes())
        self.assertTrue((self.live/"fullchain.pem").read_bytes() == (self.fanout/"cert.pem").read_bytes(), "actual Coturn hook must consume active generation")
        self.assertTrue((self.live/"privkey.pem").read_bytes() == (self.fanout/"key.pem").read_bytes())
        self.assertEqual(0o600, stat.S_IMODE((self.live/"privkey.pem").stat().st_mode))
        retained = []
        for cert in (self.root/"live").rglob("fullchain.pem"):
            key = cert.parent/"privkey.pem"
            if key.is_file() and cert.read_bytes() == (self.inputs/"previous"/"fullchain.pem").read_bytes() and key.read_bytes() == (self.inputs/"previous"/"privkey.pem").read_bytes():
                retained.append(cert.parent)
        self.assertTrue(retained, "successful activation must retain previous complete generation for recovery")

    def failure(self, mode, fault):
        rc, output = self.invoke(mode, fault)
        self.assertTrue((self.root/"fault-fired").is_file(), "injected second write must be reached")
        self.assertNotEqual(0, rc, "failed replacement must refuse completion")
        self.assertNotIn("Certificate source and coturn delivery complete.", output)
        self.assert_previous_pair()
        self.assertNotIn("consumer-observed-mixed-pair", (self.root/"effects").read_text(), "even interrupted staging must preserve a coherent live pair")
        self.assertNotIn("coturn-discovery", (self.root/"effects").read_text(), "failed staging must not deliver")

    def test_import_second_write_failure_preserves_previous_pair(self):
        self.failure("import", "import-second-fail")

    def test_import_second_write_interruption_preserves_previous_pair(self):
        self.failure("import", "import-second-interrupt")

    def test_local_partial_generation_failure_preserves_previous_pair(self):
        self.failure("local", "local-second-fail")

    def test_local_partial_generation_interruption_preserves_previous_pair(self):
        self.failure("local", "local-second-interrupt")

    def test_successful_import_activates_one_complete_generation_and_retains_previous(self):
        self.assert_success("import")

    def test_successful_local_activates_one_complete_generation_and_retains_previous(self):
        self.assert_success("local")

    def test_delivery_failure_restores_source_and_coturn_then_retry_succeeds(self):
        rc, output = self.invoke(fault="delivery-key-fail")
        self.assertTrue((self.root/"fault-fired").is_file())
        self.assertNotEqual(0, rc)
        self.assertNotIn("Certificate source and coturn delivery complete.", output)
        self.assert_previous_pair()
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (self.inputs/"previous"/"fullchain.pem").read_bytes(), "failed delivery must restore previous Coturn certificate")
        self.assertTrue((self.fanout/"key.pem").read_bytes() == (self.inputs/"previous"/"privkey.pem").read_bytes())
        (self.root/"effects").write_text("")
        self.assert_success("import")

    def certbot_lineage(self):
        archive = self.root/"archive"/BASE
        archive.parent.mkdir()
        shutil.move(str(self.live), str(archive))
        self.live.mkdir()
        for name in ("fullchain.pem", "privkey.pem"):
            (self.live/name).symlink_to(archive/name)
        renewal = self.root/"renewal"
        renewal.mkdir(); (renewal/(BASE+".conf")).write_text("owned-certbot-lineage-canary\n")
        return archive, renewal

    def test_delivery_interruption_restores_previous_complete_generation(self):
        rc,output = self.invoke(fault="delivery-key-interrupt")
        self.assertTrue((self.root/"fault-fired").is_file(), "delivery interruption must reach actual copy hook")
        self.assertNotEqual(0,rc)
        self.assertNotIn("Certificate source and coturn delivery complete.",output)
        self.assert_previous_pair()
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (self.inputs/"previous"/"fullchain.pem").read_bytes(), "interrupted delivery must restore previous Coturn certificate")
        self.assertTrue((self.fanout/"key.pem").read_bytes() == (self.inputs/"previous"/"privkey.pem").read_bytes())

    def test_persistent_recovery_hook_failure_preserves_source_without_claiming_delivery(self):
        rc,output = self.invoke(fault="delivery-key-persistent-fail")
        self.assertTrue((self.root/"fault-fired").is_file())
        self.assertNotEqual(0,rc)
        self.assert_previous_pair()
        self.assertNotIn("Certificate source and coturn delivery complete.",output)
        self.assertNotIn("coturn is not started; delivery is prepared for first start.",output)
        self.assertRegex(output.lower(), "recovery|recover|restore|rollback", "failed recovery must be reported explicitly")

    def test_unsupported_atomic_exchange_preserves_previous_pair_and_consumers(self):
        self.activation_refusal("activation-unsupported")

    def test_failed_atomic_exchange_preserves_previous_pair_and_consumers(self):
        self.activation_refusal("activation-fail")

    def activation_refusal(self,fault):
        rc,output = self.invoke(fault=fault)
        self.assertTrue((self.root/"fault-fired").is_file(), "atomic syscall fault must be reached")
        self.assertNotEqual(0,rc)
        self.assertIn("atomic certificate activation",output)
        self.assert_previous_pair()
        self.assertNotIn("coturn-discovery",(self.root/"effects").read_text(), "failed activation must not reach delivery")
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (self.inputs/"previous"/"fullchain.pem").read_bytes())
        self.assertTrue((self.fanout/"key.pem").read_bytes() == (self.inputs/"previous"/"privkey.pem").read_bytes())

    def test_interruption_after_atomic_exchange_recovers_using_actual_directory_state(self):
        self.exchange_interruption("activation-interrupt")

    def test_sigint_after_atomic_exchange_recovers_previous_complete_generation(self):
        self.exchange_interruption("activation-interrupt-int")

    def exchange_interruption(self,fault):
        rc,output = self.invoke(fault=fault)
        self.assertTrue((self.root/"fault-fired").is_file(), "signal must follow successful atomic syscall")
        self.assertNotEqual(0,rc)
        self.assert_previous_pair()
        self.assertNotIn("Certificate source and coturn delivery complete.",output)
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (self.inputs/"previous"/"fullchain.pem").read_bytes())
        self.assertTrue((self.fanout/"key.pem").read_bytes() == (self.inputs/"previous"/"privkey.pem").read_bytes())

    def test_untrusted_retained_lock_refuses_before_staging_or_activation(self):
        lock = self.root/"live"/("."+BASE+".lock")
        sentinel = self.root/"lock-target-sentinel"
        sentinel.write_text("owned-neighbor-canary\n")
        for fault in ("symlink","mode","owner"):
            with self.subTest(fault=fault):
                if lock.is_symlink() or lock.exists(): lock.unlink()
                if fault == "symlink": lock.symlink_to(sentinel)
                else:
                    lock.write_text("retained-lock-canary\n")
                    lock.chmod(0o666 if fault == "mode" else 0o600)
                meta = json.loads((self.root/"metadata.json").read_text())
                meta.pop(str(lock),None)
                if fault == "owner": meta[str(lock)] = [1000,0,0o600]
                (self.root/"metadata.json").write_text(json.dumps(meta))
                rc,output = self.invoke()
                self.assertEqual(1,rc)
                self.assertIn("certificate transaction lock unavailable",output)
                self.assert_previous_pair()
                self.assertEqual("",(self.root/"effects").read_text())
                self.assertEqual([],list((self.root/"live").glob(".*.generation.*")))
                self.assertEqual("owned-neighbor-canary\n",sentinel.read_text())

    def test_contended_valid_lock_refuses_without_effects_then_released_retry_succeeds(self):
        lock = self.root/"live"/("."+BASE+".lock")
        lock.write_text("owned-retained-lock-canary\n")
        lock.chmod(0o600)
        paths = [self.live/"fullchain.pem",self.live/"privkey.pem",
                 self.fanout/"cert.pem",self.fanout/"key.pem",lock]
        before = [(path.read_bytes(),path.stat().st_ino,stat.S_IMODE(path.stat().st_mode)) for path in paths]
        # The parent holds a real OS lock on this exact inode. main opens its
        # own descriptor, so only the production flock -n branch can refuse.
        with lock.open("r+b") as held:
            fcntl.flock(held.fileno(),fcntl.LOCK_EX | fcntl.LOCK_NB)
            try:
                rc,output = self.invoke()
                self.assertEqual(1,rc,"a valid lock held by another writer must refuse")
                self.assertIn("certificate transaction lock unavailable; delivery not started",output)
                self.assertTrue(before == [(path.read_bytes(),path.stat().st_ino,stat.S_IMODE(path.stat().st_mode)) for path in paths],
                                "contention must preserve live source, Coturn bytes, modes, and inodes")
                self.assertEqual("",(self.root/"effects").read_text(),"lock contention must precede staging and delivery")
                self.assertEqual([],list((self.root/"live").glob(".*.generation.*")))
                self.assertNotIn("Certificate source and coturn delivery complete.",output)
            finally:
                fcntl.flock(held.fileno(),fcntl.LOCK_UN)
        self.assert_success("import")

    def test_changed_copied_pair_missing_turn_SAN_refuses_before_activation(self):
        alternate = self.root/"copied-input"
        alternate.mkdir(mode=0o700)
        cert,key = alternate/"fullchain.pem",alternate/"privkey.pem"
        fixture_command([REAL_OPENSSL,"req","-x509","-newkey","ec","-pkeyopt",
                         "ec_paramgen_curve:P-384","-sha256","-nodes","-days","30",
                         "-subj","/CN="+BASE,"-addext","subjectAltName="+",".join(
                             "DNS:"+host for host in (BASE,"api."+BASE,"media."+BASE,"spa."+BASE)),
                         "-keyout",str(key),"-out",str(cert)])
        key.chmod(0o600); cert.chmod(0o644)
        public = fixture_command([REAL_OPENSSL,"x509","-in",str(cert),"-noout","-pubkey"])
        self.assertTrue(public == fixture_command([REAL_OPENSSL,"pkey","-in",str(key),"-pubout"]),
                        "copied fixture must be a complete matching pair")
        fixture_command([REAL_OPENSSL,"x509","-in",str(cert),"-noout","-checkend","0"])
        submitted = self.root/"next/fullchain.pem"
        checked = subprocess.run([REAL_OPENSSL,"x509","-in",str(submitted),"-noout","-checkhost","turn."+BASE],capture_output=True,timeout=30)
        self.assertEqual("Hostname turn."+BASE+" does match certificate",checked.stdout.decode().strip(),
                         "submitted pair must pass the initial TURN hostname check")
        checked = subprocess.run([REAL_OPENSSL,"x509","-in",str(cert),"-noout","-checkhost","turn."+BASE],capture_output=True,timeout=30)
        self.assertEqual("Hostname turn."+BASE+" does NOT match certificate",checked.stdout.decode().strip(),
                         "only copied bytes must omit the required TURN SAN")
        paths = [self.live/"fullchain.pem",self.live/"privkey.pem",self.fanout/"cert.pem",
                 self.fanout/"key.pem",self.root/"next/fullchain.pem",self.root/"next/privkey.pem"]
        before = [(path.read_bytes(),path.stat().st_ino,stat.S_IMODE(path.stat().st_mode)) for path in paths]
        rc,output = self.invoke(fault="import-staged-san-missing")
        self.assertTrue((self.root/"fault-fired").is_file(),"copy substitution must reach the owned candidate")
        self.assertEqual(1,rc,"staged SAN refusal must stop activation even when submitted bytes passed")
        self.assertIn("import: certificate SAN does not cover turn."+BASE,output)
        self.assertTrue(before == [(path.read_bytes(),path.stat().st_ino,stat.S_IMODE(path.stat().st_mode)) for path in paths],
                        "staged refusal must preserve submitted, live source, and Coturn bytes and metadata")
        effects = (self.root/"effects").read_text()
        self.assertIn("injected-staged-fullchain.pem",effects)
        self.assertIn("injected-staged-privkey.pem",effects)
        self.assertNotIn("coturn-discovery",effects,"invalid copied SAN must never reach delivery")
        self.assertNotIn("consumer-observed-mixed-pair",effects)
        self.assertNotIn("consumer-observed-incomplete-pair",effects)
        self.assertEqual([],list((self.root/"live").glob(".*.generation.*")),"refused candidate must be cleaned")
        self.assertNotIn("Certificate source and coturn delivery complete.",output)

    def test_writable_source_directory_refuses_before_staging_or_activation(self):
        self.live.chmod(0o777)
        rc,output = self.invoke()
        self.assertEqual(1,rc)
        self.assertIn("previous certificate generation invalid",output)
        self.assert_previous_pair()
        self.assertEqual("",(self.root/"effects").read_text())
        self.assertEqual([],list((self.root/"live").glob(".*.generation.*")))

    def test_fresh_local_missing_namespace_parent_creates_and_delivers_complete_pair(self):
        self.fresh_missing_parent("local")

    def test_fresh_import_missing_namespace_parent_creates_and_delivers_complete_pair(self):
        self.fresh_missing_parent("import")

    def fresh_missing_parent(self,mode):
        parent = self.root/("fresh-letsencrypt-"+mode)
        namespace = parent/"live"
        self.assertFalse(parent.exists(), "fresh fixture must start without the namespace parent")
        self.env["LE_LIVE_ROOT"] = str(namespace)
        self.live = namespace/BASE
        rc,output = self.invoke(mode)
        self.assertEqual(0,rc,"fresh mode must support an absent owned namespace parent: "+output)
        self.assertIn("Certificate source and coturn delivery complete.",output)
        self.assertNotIn("certbot-managed-lineage",(self.root/"effects").read_text(), "local/import fresh namespace construction needs no Certbot")
        for directory in (parent,namespace,self.live):
            checked = subprocess.run([str(self.root/"bin/stat"),"-Lc","%u:%g:%a",str(directory)],env=self.env,capture_output=True,timeout=30)
            self.assertEqual(0,checked.returncode)
            self.assertEqual("0:0:700",checked.stdout.decode().strip(),"fresh directories must have virtual root ownership and private modes")
        self.assertEqual(0o600,stat.S_IMODE((self.live/"privkey.pem").stat().st_mode))
        self.assertEqual(0o644,stat.S_IMODE((self.live/"fullchain.pem").stat().st_mode))
        self.assertTrue((self.live/"fullchain.pem").read_bytes() == (self.root/"next/fullchain.pem").read_bytes())
        self.assertTrue((self.live/"privkey.pem").read_bytes() == (self.root/"next/privkey.pem").read_bytes())
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (self.live/"fullchain.pem").read_bytes())
        self.assertTrue((self.fanout/"key.pem").read_bytes() == (self.live/"privkey.pem").read_bytes())
        effects = (self.root/"effects").read_text()
        self.assertNotIn("consumer-observed-incomplete-pair",effects)
        self.assertNotIn("consumer-observed-mixed-pair",effects)

    def expire_previous_generation(self):
        csr = self.root/"previous.csr"
        fixture_command([REAL_OPENSSL,"req","-new","-key",str(self.live/"privkey.pem"),
                         "-subj","/CN="+BASE,"-out",str(csr)])
        fixture_command([REAL_OPENSSL,"x509","-req","-in",str(csr),"-signkey",
                         str(self.live/"privkey.pem"),"-days","0","-out",str(self.live/"fullchain.pem")])
        checked = subprocess.run([REAL_OPENSSL,"x509","-in",str(self.live/"fullchain.pem"),"-noout","-checkend","0"],capture_output=True)
        self.assertNotEqual(0,checked.returncode,"previous fixture must be independently expired")
        return (self.live/"fullchain.pem").read_bytes(), (self.live/"privkey.pem").read_bytes()

    def test_trusted_expired_previous_generation_can_be_replaced_and_retained(self):
        previous_cert, previous_key = self.expire_previous_generation()
        rc,output = self.invoke()
        self.assertEqual(0,rc,output)
        self.assertTrue((self.live/"fullchain.pem").read_bytes() == (self.root/"next/fullchain.pem").read_bytes())
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (self.root/"next/fullchain.pem").read_bytes())
        retained = [p for p in (self.root/"live").rglob("fullchain.pem") if p.read_bytes() == previous_cert and (p.parent/"privkey.pem").read_bytes() == previous_key]
        self.assertTrue(retained,"expired but complete previous generation must be retained")

    def test_failed_delivery_restores_expired_source_and_reports_failed_recovery_truthfully(self):
        previous_cert,previous_key = self.expire_previous_generation()
        rc,output = self.invoke(fault="delivery-key-fail")
        self.assertTrue((self.root/"fault-fired").is_file())
        self.assertNotEqual(0,rc)
        self.assertTrue((self.live/"fullchain.pem").read_bytes() == previous_cert,"failed delivery must restore expired previous source certificate")
        self.assertTrue((self.live/"privkey.pem").read_bytes() == previous_key)
        self.assertRegex(output.lower(),"recovery failed|recovery unavailable")
        self.assertNotIn("Certificate source and coturn delivery complete.",output)

    def test_import_refuses_certbot_owned_lineage_without_overwriting_archive(self):
        archive, renewal = self.certbot_lineage()
        before = {str(p):p.read_bytes() for root in (archive, renewal) for p in root.rglob("*") if p.is_file()}
        rc, output = self.invoke()
        self.assertNotEqual(0, rc, "import must not replace a Certbot-owned lineage")
        self.assertRegex(output.lower(), "certbot|managed|lineage")
        self.assertTrue(before == {str(p):p.read_bytes() for root in (archive, renewal) for p in root.rglob("*") if p.is_file()}, "Certbot archive and renewal bytes must be preserved")
        self.assertTrue(all((self.live/name).is_symlink() for name in ("fullchain.pem", "privkey.pem")))

    def test_certbot_existing_lineage_control_delivers_without_replacing_links(self):
        archive, renewal = self.certbot_lineage()
        links = [os.readlink(self.live/name) for name in ("fullchain.pem", "privkey.pem")]
        rc, output = self.invoke("letsencrypt")
        self.assertEqual(0, rc, output)
        self.assertIn("certbot-managed-lineage", (self.root/"effects").read_text())
        self.assertEqual(links, [os.readlink(self.live/name) for name in ("fullchain.pem", "privkey.pem")])
        self.assertTrue((self.fanout/"cert.pem").read_bytes() == (archive/"fullchain.pem").read_bytes())
        self.assertTrue((renewal/(BASE+".conf")).is_file())

    def test_nonroot_refusal_control_precedes_replacement_and_delivery(self):
        rc, output = self.invoke(euid=1000)
        self.assertEqual(1, rc)
        self.assertIn("root required; certificate delivery not started", output)
        self.assert_previous_pair()
        self.assertEqual("", (self.root/"effects").read_text())


unittest.main(verbosity=2)
