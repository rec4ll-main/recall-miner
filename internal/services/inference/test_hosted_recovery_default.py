"""Hosted recovery policy and operator reconciliation (synthetic, offline).

The Chutes sidecar always uses carried-unknown-aging-v1 (five minutes), so unknown outcomes stop
holding admission once they age out; HOSTED_RECOVERY_POLICY and HOSTED_UNKNOWN_AGE_OUT_SECONDS
are retired and ignored there (test_chutes_max_concurrent.py). legacy and carried-unknown-v1
stay in the ledger library for older ledgers and the operator CLI. A stuck pending attempt can
be reconciled by the operator: it becomes terminal with outcome 'unknown' and its possible-spend
record is kept. Synthetic disposable ledgers only.
"""
import io
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

import hosted_accounting as ha

HERE = os.path.dirname(os.path.abspath(__file__))
BODY = "ab" * 32


def age_pending(path, seconds):
    with sqlite3.connect(path) as conn:
        conn.execute("UPDATE hosted_attempts SET created=datetime('now', ?) WHERE state='pending'",
                     (f"-{seconds} seconds",))


def unknown(ledger, key):
    """One dispatched attempt left with an unknown outcome (timeout after send, crash); returns its id."""
    attempt = ledger.begin(key, BODY)
    ledger.mark_sent(attempt)
    ledger.finish_local(attempt)
    return attempt


def cli(*argv):
    out, err = io.StringIO(), io.StringIO()
    with redirect_stdout(out), redirect_stderr(err):
        code = ha.main(list(argv))
    return code, out.getvalue(), err.getvalue()


class ServiceDefaults(unittest.TestCase):
    """service.py wiring, in a clean process with a synthetic ledger and key."""

    SCRIPT = (
        'import sys,types,json,sqlite3\n'
        'sys.modules.setdefault("sentence_transformers",types.SimpleNamespace(CrossEncoder=object,SentenceTransformer=object))\n'
        'sys.modules.setdefault("transformers",types.SimpleNamespace(AutoModelForCausalLM=object,AutoTokenizer=object))\n'
        'import service\n'
        'L = service.HOSTED_LEDGER\n'
        'out = {"error": service.HOSTED_LEDGER_ERROR}\n'
        'if L is not None:\n'
        '    out.update(policy=L.recovery_policy, bound=L.max_outstanding, age=L.unknown_age_out_seconds,\n'
        '               concurrent=L.max_concurrent)\n'
        '    if "exercise" in sys.argv:\n'
        '        body = "ab" * 32\n'
        '        def unknown(k):\n'
        '            a = L.begin(k, body); L.mark_sent(a); L.finish_local(a)\n'
        '        n = 0\n'
        '        try:\n'
        '            while n < 5:\n'
        '                unknown("u%d" % n); n += 1\n'
        '        except service.HostedAdmissionBlocked:\n'
        '            pass\n'
        '        out["unknowns_admitted"] = n\n'
        '        out["ready_while_held"] = service.hosted_available()\n'
        '        with sqlite3.connect(L.path) as c:\n'
        '            c.execute("UPDATE hosted_attempts SET created=datetime(\'now\', \'-3601 seconds\')")\n'
        '        out["ready_after_age_out"] = service.hosted_available()\n'
        '        L.begin("after", body)\n'
        '        out["pending_kept"] = len(L.pending())\n'
        'print(json.dumps(out))\n')

    def run_service(self, extra=None, *args):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "l.sqlite")
            lid = ha.HostedLedger.initialize(path)
            key = os.path.join(d, "key")
            with open(key, "w") as f:
                f.write("synthetic-test-key\n")
            os.chmod(key, 0o600)
            env = {"PATH": os.environ.get("PATH", ""), "HOME": d, "GEN_CHUTES_ENABLED": "true",
                   "CHUTES_MODEL": "synthetic-org/synthetic-model", "CHUTES_API_KEY_FILE": key,
                   "CHUTES_LEDGER_PATH": path, "CHUTES_LEDGER_ID": lid}
            env.update(extra or {})
            if os.environ.get("PYTHONPATH"):
                env["PYTHONPATH"] = os.environ["PYTHONPATH"]
            r = subprocess.run([sys.executable, "-c", self.SCRIPT, *args], cwd=HERE, env=env,
                               capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout.strip().splitlines()[-1])

    AGING = {"error": "", "policy": ha.AGING_POLICY, "bound": 2, "age": 300, "concurrent": 4}

    def test_default_is_aging_policy(self):
        self.assertEqual(self.run_service(), self.AGING)

    def test_empty_values_mean_unset(self):
        out = self.run_service({"HOSTED_RECOVERY_POLICY": "", "HOSTED_MAX_OUTSTANDING": "",
                                "HOSTED_UNKNOWN_AGE_OUT_SECONDS": ""})
        self.assertEqual(out, self.AGING)

    def test_other_policies_are_no_longer_selectable(self):
        # Retired settings: the sidecar warns and keeps the one-hour brake.
        for extra in ({"HOSTED_RECOVERY_POLICY": "legacy"}, {"HOSTED_RECOVERY_POLICY": "carried-unknown-v1"},
                      {"HOSTED_UNKNOWN_AGE_OUT_SECONDS": "300"}):
            with self.subTest(**extra):
                self.assertEqual(self.run_service(extra), self.AGING)

    def test_formerly_invalid_settings_no_longer_block_generation(self):
        for extra in ({"HOSTED_RECOVERY_POLICY": "none"}, {"HOSTED_MAX_OUTSTANDING": "1"},
                      {"HOSTED_UNKNOWN_AGE_OUT_SECONDS": "299"}):
            with self.subTest(**extra):
                self.assertEqual(self.run_service(extra), self.AGING)

    def test_default_policy_recovers_after_age_out(self):
        out = self.run_service(None, "exercise")
        # Two carried unknowns plus the third that closes admission, as for carried-unknown-v1.
        self.assertEqual(out["unknowns_admitted"], 3)
        self.assertFalse(out["ready_while_held"])
        self.assertTrue(out["ready_after_age_out"])
        # The aged unknowns stay pending (the spend record is kept): 3 rows, plus the new attempt.
        self.assertEqual(out["pending_kept"], 4)


class LegacyStillBlocks(unittest.TestCase):
    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.path = os.path.join(d.name, "l.sqlite")
        self.lid = ha.HostedLedger.initialize(self.path)

    def test_explicit_legacy_blocks_across_restart_regardless_of_age(self):
        ledger = ha.HostedLedger.open_existing(self.path, self.lid, recovery_policy="legacy")
        unknown(ledger, "k1")
        self.assertFalse(ledger.available())
        with self.assertRaises(ha.HostedAdmissionBlocked):
            ledger.begin("k2", BODY)
        age_pending(self.path, 30 * 86400)
        restarted = ha.HostedLedger.open_existing(self.path, self.lid, recovery_policy="legacy")
        self.assertFalse(restarted.available())
        with self.assertRaises(ha.HostedAdmissionBlocked):
            restarted.begin("k3", BODY)


class Reconcile(unittest.TestCase):
    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.path = os.path.join(d.name, "l.sqlite")
        self.lid = ha.HostedLedger.initialize(self.path)

    def rows(self, sql, *args):
        with sqlite3.connect(self.path) as conn:
            return conn.execute(sql, args).fetchall()

    def test_reconcile_by_request_key_resumes_admission_and_keeps_the_record(self):
        # A running legacy service: one unknown outcome blocks it.
        service = ha.HostedLedger.open_existing(self.path, self.lid)
        stuck = unknown(service, "api:stuck")
        self.assertFalse(service.available())
        # Too recent: refused, nothing changed.
        code, _, err = cli("reconcile", self.path, self.lid, "--request-key", "api:stuck")
        self.assertEqual(code, 1)
        self.assertIn("too recent", err)
        self.assertEqual(service.pending(), [stuck])
        age_pending(self.path, ha.MIN_RECONCILE_AGE_SECONDS + 1)
        code, out, _ = cli("reconcile", self.path, self.lid, "--request-key", "api:stuck")
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out), {"reconciled": [stuck], "outcome": "unknown"})
        # Terminal with an unknown outcome; attempt and request rows (sent marker) kept;
        # no provider outcome invented.
        self.assertEqual(self.rows("SELECT state FROM hosted_attempts WHERE id=?", stuck), [("complete",)])
        self.assertEqual(self.rows("SELECT outcome FROM hosted_attempt_reconciliations WHERE id=?", stuck),
                         [("unknown",)])
        self.assertEqual(self.rows("SELECT request_key, sent FROM hosted_attempt_requests WHERE id=?", stuck),
                         [("api:stuck", 1)])
        self.assertEqual(self.rows("SELECT count(*) FROM hosted_attempt_outcomes"), [(0,)])
        status = ha.HostedLedger.status(self.path, self.lid)
        self.assertEqual((status["pending"], status["reconciled_unknown"]), ([], [stuck]))
        # The same running process resumes admission: the forward move is not a rollback.
        self.assertTrue(service.available())
        nxt = service.begin("api:next", BODY)
        service.complete(nxt, http_status=200, usage=(1, 1))
        self.assertTrue(service.available())
        # The key stays used: a reconciled request is never re-dispatched.
        with self.assertRaises(ha.HostedRequestKeyConflict):
            service.begin("api:stuck", BODY)
        # And a restarted process sees the same.
        self.assertTrue(ha.HostedLedger.open_existing(self.path, self.lid).available())

    def test_reconcile_all_older_than_leaves_recent_attempts(self):
        ledger = ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2,
                                               recovery_policy="carried-unknown-v1")
        old = [unknown(ledger, "old-%d" % i) for i in range(3)]
        self.assertFalse(ledger.available())
        age_pending(self.path, 7200)
        with self.assertRaises(ha.HostedAdmissionBlocked):
            unknown(ledger, "blocked")
        code, out, _ = cli("reconcile", self.path, self.lid, "--all-pending-older-than", "3600")
        self.assertEqual(code, 0)
        self.assertEqual(sorted(json.loads(out)["reconciled"]), sorted(old))
        self.assertEqual(self.rows("SELECT phase FROM hosted_attempt_phases WHERE id IN (?,?,?)", *old),
                         [("terminal",)] * 3)
        fresh = unknown(ledger, "fresh")
        code, out, _ = cli("reconcile", self.path, self.lid, "--all-pending-older-than", "3600")
        self.assertEqual((code, json.loads(out)["reconciled"]), (0, []))
        self.assertEqual(ledger.pending(), [fresh])

    def test_reconcile_by_attempt_id(self):
        ledger = ha.HostedLedger.open_existing(self.path, self.lid)
        stuck = unknown(ledger, "k")
        age_pending(self.path, 600)
        self.assertEqual(ledger.reconcile_unknown(attempt=stuck), [stuck])
        self.assertTrue(ledger.available())

    def test_refusals(self):
        ledger = ha.HostedLedger.open_existing(self.path, self.lid)
        done = ledger.begin("done", BODY)
        ledger.mark_sent(done)
        ledger.complete(done, http_status=200, usage=(1, 1))
        age_pending(self.path, 600)
        for kwargs in ({"request_key": "done"}, {"request_key": "missing"}, {"attempt": done}):
            with self.subTest(**kwargs), self.assertRaises(ha.HostedAdmissionBlocked):
                ledger.reconcile_unknown(**kwargs)
        for kwargs in ({}, {"older_than": 299}, {"older_than": True}, {"attempt": "a", "request_key": "k"}):
            with self.subTest(**kwargs), self.assertRaises(ha.HostedAdmissionBlocked):
                ledger.reconcile_unknown(**kwargs)
        self.assertEqual(self.rows("SELECT outcome FROM hosted_attempt_outcomes"), [("validated",)])
        self.assertEqual(cli("reconcile", self.path, self.lid, "--all-pending-older-than", "abc")[0], 2)
        self.assertEqual(cli("reconcile", self.path, self.lid, "--everything", "1")[0], 2)
        self.assertEqual(cli("reconcile", self.path, "wrong-id", "--attempt", done)[0], 1)

    def test_own_active_attempt_is_never_reconciled(self):
        ledger = ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2,
                                               recovery_policy="carried-unknown-v1")
        active = ledger.begin("active", BODY)
        ledger.mark_sent(active)
        age_pending(self.path, 600)
        with self.assertRaises(ha.HostedAdmissionBlocked):
            ledger.reconcile_unknown(attempt=active)
        self.assertEqual(ledger.pending(), [active])


if __name__ == "__main__":
    unittest.main()
