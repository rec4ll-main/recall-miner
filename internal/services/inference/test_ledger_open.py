"""How the sidecar opens its ledger (synthetic, offline).

CHUTES_LEDGER_ID is optional. Empty, unset or the old template value pending-init: the
sidecar creates the ledger in its state directory on first start and opens the one it
finds after that. A ledger id (UUID) pins the ledger, and a different file is refused as an
identity mismatch. A missing state directory, a non-regular or unreadable ledger file and a
malformed id are refused; embedding and rerank keep serving. Disposable ledgers and a
synthetic key only; nothing is dispatched.
"""
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import unittest

import hosted_accounting as ha

HERE = os.path.dirname(os.path.abspath(__file__))
INVALID = "CHUTES_LEDGER_ID must be a ledger id (UUID) or left empty"
OTHER_ID = "00000000-0000-4000-8000-000000000001"

# Clean process with stubbed model libraries and fake embed/rerank models; serves
# /embed, /rerank and /generate in-process on loopback.
SCRIPT = (
    'import sys,types,json,threading,urllib.error,urllib.request\n'
    'sys.modules.setdefault("sentence_transformers",types.SimpleNamespace(CrossEncoder=object,SentenceTransformer=object))\n'
    'sys.modules.setdefault("transformers",types.SimpleNamespace(AutoModelForCausalLM=object,AutoTokenizer=object))\n'
    'import service\n'
    'from http.server import ThreadingHTTPServer\n'
    'class Vecs(list):\n'
    ' def tolist(self):return list(self)\n'
    'class Emb:\n'
    ' def encode(self,texts,**k):return Vecs([[1.0,0.0] for _ in texts])\n'
    'class Rr:\n'
    ' def predict(self,pairs,**k):return [float(i) for i,_ in enumerate(pairs)]\n'
    'service.embedder=Emb();service.reranker=Rr()\n'
    'srv=ThreadingHTTPServer(("127.0.0.1",0),service.Handler);port=srv.server_address[1]\n'
    'threading.Thread(target=srv.serve_forever,daemon=True).start()\n'
    'def post(path,body):\n'
    ' req=urllib.request.Request("http://127.0.0.1:%d%s"%(port,path),data=json.dumps(body).encode(),headers={"Content-Type":"application/json"})\n'
    ' try:\n'
    '  with urllib.request.urlopen(req,timeout=10) as r:return r.status\n'
    ' except urllib.error.HTTPError as e:return e.code\n'
    'out={"enabled":service.CHUTES_CONFIG.enabled,"error":service.HOSTED_LEDGER_ERROR,\n'
    ' "ledger":service.HOSTED_LEDGER is not None,"readiness":list(service.generation_readiness())}\n'
    'if "serve" in sys.argv:\n'
    ' out.update(embed=post("/embed",{"inputs":["a"]}),rerank=post("/rerank",{"query":"q","texts":["x","y"]}),\n'
    '  generate=post("/generate",{"query":"q","contexts":[{"id":"1","text":"t"}]}))\n'
    'srv.shutdown()\n'
    'print(json.dumps(out))\n')



def digest(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


class LedgerOpen(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = tmp.name
        self.state = os.path.join(self.dir, "state")
        os.mkdir(self.state)
        self.path = os.path.join(self.state, "hosted.sqlite")
        self.key = os.path.join(self.dir, "api-key")
        with open(self.key, "w") as f:
            f.write("synthetic-test-key\n")
        os.chmod(self.key, 0o600)

    def run_service(self, ledger_id, *args, path=None):
        env = {"PATH": os.environ.get("PATH", ""), "HOME": self.dir, "GENERATION_CONNECTOR": "chutes",
               "GEN_CHUTES_ENABLED": "true", "CHUTES_MODEL": "synthetic-org/synthetic-model",
               "CHUTES_API_KEY_FILE": self.key, "CHUTES_LEDGER_PATH": path or self.path}
        if ledger_id is not None:
            env["CHUTES_LEDGER_ID"] = ledger_id
        if os.environ.get("PYTHONPATH"):
            env["PYTHONPATH"] = os.environ["PYTHONPATH"]
        r = subprocess.run([sys.executable, "-c", SCRIPT, *args], cwd=HERE, env=env,
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr)  # the process starts either way
        return json.loads(r.stdout.strip().splitlines()[-1]), r.stderr

    def assert_ready(self, out):
        self.assertEqual((out["error"], out["ledger"]), ("", True))
        self.assertEqual(out["readiness"], [True, ""])

    def assert_refused(self, out, reason):
        self.assertTrue(out["enabled"])
        self.assertFalse(out["ledger"])
        self.assertEqual(out["error"], reason)
        self.assertEqual(out["readiness"], [False, reason])

    def test_first_start_creates_the_ledger_without_an_id(self):
        for value in (None, "", "pending-init", "  "):
            with self.subTest(value=value):
                if os.path.exists(self.path):
                    os.unlink(self.path)
                out, stderr = self.run_service(value, "serve")
                self.assert_ready(out)
                self.assertIn("hosted_ledger_created", stderr)
                self.assertNotIn("generation_refused", stderr)
                self.assertTrue(os.path.isfile(self.path))
                self.assertEqual((out["embed"], out["rerank"]), (200, 200))

    def test_later_starts_reuse_the_same_ledger(self):
        self.run_service("")
        first = ha.HostedLedger.stored_identity(self.path)
        out, stderr = self.run_service("")
        self.assert_ready(out)
        self.assertNotIn("hosted_ledger_created", stderr)
        self.assertEqual(ha.HostedLedger.stored_identity(self.path), first)
        self.assertEqual([n for n in os.listdir(self.state) if n.endswith(".sqlite")], ["hosted.sqlite"])

    def test_ledger_made_with_init_opens_without_an_id(self):
        lid = ha.HostedLedger.initialize(self.path)
        out, stderr = self.run_service("pending-init")
        self.assert_ready(out)
        self.assertNotIn("hosted_ledger_created", stderr)
        self.assertEqual(ha.HostedLedger.stored_identity(self.path), lid)

    def test_matching_ledger_id_is_admitted(self):
        lid = ha.HostedLedger.initialize(self.path)
        out, stderr = self.run_service(lid)
        self.assert_ready(out)
        self.assertNotIn("generation_refused", stderr)

    def test_set_id_never_creates_a_ledger(self):
        out, _ = self.run_service(OTHER_ID)
        self.assert_refused(out, "hosted ledger missing; refusing to create a new one")
        self.assertFalse(os.path.exists(self.path))

    def test_valid_but_different_ledger_id_is_identity_mismatch(self):
        ha.HostedLedger.initialize(self.path)
        out, _ = self.run_service(OTHER_ID)
        self.assert_refused(out, "hosted ledger identity mismatch")

    def test_malformed_id_is_refused_and_creates_nothing(self):
        out, stderr = self.run_service("not-a-ledger-id", "serve")
        self.assert_refused(out, INVALID)
        self.assertIn("generation_refused", stderr)
        self.assertEqual(out["embed"], 200)
        self.assertNotEqual(out["generate"], 200)
        self.assertFalse(os.path.exists(self.path))

    def test_missing_state_directory_is_refused(self):
        missing = os.path.join(self.dir, "absent", "hosted.sqlite")
        out, _ = self.run_service("", path=missing)
        self.assert_refused(out, "hosted ledger directory missing; refusing to create it")
        self.assertFalse(os.path.exists(os.path.dirname(missing)))

    def test_corrupt_ledger_is_refused_and_left_alone(self):
        with open(self.path, "wb") as f:
            f.write(b"not a database" * 100)
        before = digest(self.path)
        out, _ = self.run_service("")
        self.assert_refused(out, "hosted ledger unreadable")
        self.assertEqual(digest(self.path), before)

    def test_symlinked_ledger_is_refused(self):
        real = os.path.join(self.dir, "elsewhere.sqlite")
        ha.HostedLedger.initialize(real)
        os.symlink(real, self.path)
        out, _ = self.run_service("")
        self.assert_refused(out, "hosted ledger is not a regular file")

    @unittest.skipIf(hasattr(os, "geteuid") and os.geteuid() == 0, "root ignores directory modes")
    def test_unwritable_state_directory_is_refused(self):
        os.chmod(self.state, 0o500)
        self.addCleanup(os.chmod, self.state, 0o700)
        out, _ = self.run_service("")
        self.assert_refused(out, "hosted ledger or journal directory not writable by runtime UID")
        self.assertFalse(os.path.exists(self.path))


class OpenOrCreate(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.path = os.path.join(tmp.name, "hosted.sqlite")

    def test_creates_once_then_reopens(self):
        a = ha.HostedLedger.open_or_create(self.path)
        self.assertTrue(a.created)
        b = ha.HostedLedger.open_or_create(self.path)
        self.assertFalse(b.created)
        self.assertEqual(a.expected_id, b.expected_id)
        self.assertEqual(ha.HostedLedger.stored_identity(self.path), a.expected_id)

    def test_failed_create_leaves_no_file(self):
        orig = ha.HostedLedger.connect
        def boom(self, *a, **k):
            raise ha.HostedAdmissionBlocked("synthetic failure")
        ha.HostedLedger.connect = boom
        try:
            with self.assertRaises(ha.HostedAdmissionBlocked):
                ha.HostedLedger.open_or_create(self.path)
        finally:
            ha.HostedLedger.connect = orig
        self.assertFalse(os.path.exists(self.path))
        self.assertTrue(ha.HostedLedger.open_or_create(self.path).created)

    def test_cli_status_and_reconcile_without_id(self):
        lid = ha.HostedLedger.initialize(self.path)
        run = lambda *a: subprocess.run([sys.executable, "hosted_accounting.py", *a], cwd=HERE,
                                        capture_output=True, text=True, timeout=60)
        r = run("status", self.path)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout)["ledger_id"], lid)
        r = run("status", self.path, lid)
        self.assertEqual(r.returncode, 0, r.stderr)
        r = run("reconcile", self.path, "--all-pending-older-than", "3600")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout)["reconciled"], [])
        r = run("status", os.path.join(os.path.dirname(self.path), "absent.sqlite"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("hosted ledger missing", r.stderr)


if __name__ == "__main__":
    unittest.main()
