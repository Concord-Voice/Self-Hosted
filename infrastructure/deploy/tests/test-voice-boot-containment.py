#!/usr/bin/env python3
"""Descriptor lifetimes of the real voice-boot-containment.py AssociationStore."""
import importlib.util
import os
from pathlib import Path
import shutil
import tempfile
import unittest
import unittest.mock

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("voice_boot_containment", HERE.parent / "voice-boot-containment.py")
containment = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(containment)


def open_count():
    return len(os.listdir("/dev/fd"))


class AssociationStoreDescriptors(unittest.TestCase):
    def setUp(self):
        # The store refuses group- or world-writable ancestry (such as /tmp),
        # so the fixture lives beside this file, under the owner's checkout.
        self.root = Path(tempfile.mkdtemp(dir=HERE))
        self.addCleanup(shutil.rmtree, self.root)
        self.root.chmod(0o755)
        for part in self.root.parents:
            info = part.stat()
            if info.st_mode & 0o022 or info.st_uid not in (0, os.geteuid()):
                if os.environ.get("CI"):
                    self.fail(f"untrusted ancestry {part}: CI must run this suite")
                self.skipTest(f"untrusted ancestry {part}")
        self.lock = self.root / ".deploy-transaction.lock"
        self.lock.touch(mode=0o600)

    def test_open_close_and_recheck_balance(self):
        before = open_count()
        store = containment.AssociationStore(str(self.lock))
        pinned = open_count()
        self.assertGreater(pinned, before)  # positive control: setup pins descriptors
        for _ in range(3):
            store.recheck()
        self.assertEqual(open_count(), pinned)
        store.close()
        self.assertEqual(open_count(), before)

    def test_refused_setup_closes_what_it_opened(self):
        self.lock.chmod(0o666)
        before = open_count()
        with self.assertRaisesRegex(containment.Refusal, "untrusted deployment lock"):
            containment.AssociationStore(str(self.lock))
        self.assertEqual(open_count(), before)

    def test_refused_recheck_keeps_pinned_and_closes_its_own(self):
        store = containment.AssociationStore(str(self.lock))
        self.addCleanup(store.close)
        pinned = open_count()
        identities = iter([(1, 1), (2, 2)])
        with unittest.mock.patch.object(containment, "namespace_identity", lambda _fd: next(identities)):
            with self.assertRaisesRegex(containment.Refusal, "deployment parent replaced"):
                store.recheck()
        self.assertEqual(open_count(), pinned)
        store.recheck()  # the pinned descriptors are still valid


if __name__ == "__main__":
    unittest.main()
