"""v2p5: a post-dispatch retry is not blocked by its own stalled first attempt (synthetic, offline).

The ":r1" retry may pass begin(exempt_attempt=<first attempt>): that one pending row, recorded
under exactly the retry key minus ":r1", is left out of U for that admission only. Every other
case is decided as without the exemption. A local plain-HTTP stub on 127.0.0.1 stands in for the
provider (same technique as test_retry.py); no network, no model, fixture keys only.
"""
import contextlib
import io
import json
import os
import sqlite3
import sys
import tempfile
import threading
import time
import types
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

for name, attrs in [("sentence_transformers", ["CrossEncoder", "SentenceTransformer"]),
                    ("transformers", ["AutoModelForCausalLM", "AutoTokenizer"])]:
    module = types.ModuleType(name)
    for attr in attrs:
        setattr(module, attr, type(attr, (), {}))
    sys.modules[name] = module
sys.path.insert(0, os.environ.get("CANDIDATE_DIR", os.path.dirname(os.path.abspath(__file__))))
import service as s  # noqa: E402
from concurrency import GenerationLimiter, reset_generation_limiter_for_tests  # noqa: E402
from hosted_accounting import AGING_POLICY, HostedAdmissionBlocked, HostedLedger  # noqa: E402
from hosted_provider import ChutesConfig  # noqa: E402

BODY = "ef" * 32  # synthetic sha256-shaped digest
POLICY = "carried-unknown-v1"
OUTPUT = json.dumps({"model": "fixture/model", "choices": [{"finish_reason": "stop", "message": {
    "content": "final answer"}}], "usage": {"prompt_tokens": 2, "completion_tokens": 3}}).encode()


class Provider:
    """Scripted stub: each POST takes the next behaviour ('ok' or 'hang')."""

    def __init__(self, script):
        self.script, self.bodies, self.release = list(script), [], threading.Event()
        provider = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                provider.bodies.append(self.rfile.read(int(self.headers["Content-Length"])))
                kind = provider.script.pop(0) if provider.script else "ok"
                self.close_connection = True
                if kind == "hang":
                    provider.release.wait(10)
                    return
                self.connection.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: " + str(len(OUTPUT)).encode()
                                        + b"\r\nConnection: close\r\n\r\n" + OUTPUT)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(5)


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.path = tmp.name + "/ledger.sqlite"
        self.lid = HostedLedger.initialize(self.path)
        self.l = self.open()
        self.addCleanup(self.release_all)

    def open(self, policy=POLICY, n=5, age=None):
        return HostedLedger.open_existing(self.path, self.lid, max_outstanding=2, recovery_policy=policy,
                                          unknown_age_out_seconds=age, max_concurrent=n)

    def release_all(self):
        for attempt in list(self.l._owned_attempts()):
            self.l.finish_local(attempt)

    def unknown(self, key, ledger=None):
        """A sent attempt released locally while still pending: an unknown that counts in U."""
        ledger = ledger or self.l
        attempt = ledger.begin(key, BODY)
        ledger.mark_sent(attempt)
        ledger.finish_local(attempt)
        return attempt

    def rows(self):
        with contextlib.closing(sqlite3.connect("file:%s?mode=ro" % self.path, uri=True)) as c:
            return c.execute("SELECT a.state, r.request_key, p.phase FROM hosted_attempts a "
                             "JOIN hosted_attempt_requests r USING(id) LEFT JOIN hosted_attempt_phases p "
                             "USING(id) ORDER BY a.rowid").fetchall()

    def state(self, attempt):
        with contextlib.closing(sqlite3.connect("file:%s?mode=ro" % self.path, uri=True)) as c:
            return c.execute("SELECT a.state, p.phase FROM hosted_attempts a LEFT JOIN hosted_attempt_phases p "
                             "USING(id) WHERE a.id=?", (attempt,)).fetchone()

    def refused(self, key, **kw):
        with self.assertRaises(HostedAdmissionBlocked) as cm:
            self.l.begin(key, BODY, **kw)
        self.assertEqual(str(cm.exception), "hosted completion unresolved; no new dispatch")


class LedgerExemption(Base):
    def setUp(self):
        super().setUp()
        self.carried = [self.unknown("c-0"), self.unknown("c-1")]
        self.first = self.unknown("k1")   # the stalled first attempt: U=3

    def test_retry_admitted_only_with_exemption(self):
        self.refused("k1:r1")                                   # v2p4 behaviour: the third unknown refuses
        retry = self.l.begin("k1:r1", BODY, exempt_attempt=self.first)
        self.assertIn(retry, self.l._owned_attempts())
        self.assertEqual(self.state(self.first), ("pending", "unknown"))   # row unchanged
        self.assertEqual([r[:2] for r in self.rows()], [("pending", "c-0"), ("pending", "c-1"),
                                                        ("pending", "k1"), ("pending", "k1:r1")])

    def test_exemption_is_one_shot(self):
        retry = self.l.begin("k1:r1", BODY, exempt_attempt=self.first)
        self.refused("fresh")                                   # U=3 counts again for everyone else
        self.refused("fresh", exempt_attempt=self.first)        # not that request's retry key
        self.l.complete(retry)
        self.l.finish_local(retry)
        self.refused("fresh")
        self.assertFalse(self.l.available())
        self.assertEqual(self.state(self.first), ("pending", "unknown"))

    def test_retry_that_stalls_too_leaves_four_unknowns_and_nothing_more(self):
        retry = self.l.begin("k1:r1", BODY, exempt_attempt=self.first)
        self.l.mark_sent(retry)
        self.l.finish_local(retry)                              # U=4: 2 carried + both attempts of k1
        self.refused("fresh")
        self.refused("k1:r1:r1", exempt_attempt=retry)          # exempts one row only: U-1=3 still refuses
        self.refused("k2:r1", exempt_attempt=self.first)

    def test_key_mismatch_ignored(self):
        self.refused("k2:r1", exempt_attempt=self.first)        # another request's retry
        self.refused("c-0:r1", exempt_attempt=self.first)       # key of a different pending row
        self.refused("k1x", exempt_attempt=self.first)          # not a retry key
        self.refused("k1:r2", exempt_attempt=self.first)
        self.refused("K1:r1", exempt_attempt=self.first)        # exact match only
        self.refused("k1:r1", exempt_attempt=self.carried[0])   # retry key, but not its first attempt

    def test_unknown_or_invalid_id_ignored(self):
        for bad in ("no-such-attempt", "", 123, b"x", self.first.upper()):
            with self.subTest(bad=bad):
                self.refused("k1:r1", exempt_attempt=bad)
        self.assertEqual(len(self.rows()), 3)                   # nothing reserved by a refusal


class NotPending(Base):
    def test_completed_first_attempt_is_not_exempt(self):
        first = self.l.begin("k1", BODY)
        self.l.mark_sent(first)
        self.l.complete(first)                                  # terminal: not pending
        self.l.finish_local(first)
        self.unknown("c-0"), self.unknown("c-1"), self.unknown("c-2")   # U=3 without k1
        self.refused("k1:r1")
        self.refused("k1:r1", exempt_attempt=first)
        self.assertEqual(self.state(first)[0], "complete")

    def test_aged_first_attempt_is_not_subtracted_twice(self):
        self.l = self.open(policy=AGING_POLICY, age=300)
        first = self.unknown("k1")
        with contextlib.closing(sqlite3.connect(self.path)) as c, c:
            c.execute("UPDATE hosted_attempts SET created=datetime('now','-1 hour') WHERE id=?", (first,))
        carried = [self.unknown("c-%d" % i) for i in range(3)]
        self.refused("k1:r1")                                 # 3 fresh carried; first already aged out
        self.refused("k1:r1", exempt_attempt=first)             # exempting an uncounted row frees nothing
        self.assertEqual(len(carried), 3)

    def test_aged_carried_with_exemption(self):
        self.l = self.open(policy=AGING_POLICY, age=300)
        self.unknown("c-0"), self.unknown("c-1")
        first = self.unknown("k1")
        self.refused("k1:r1")
        self.assertIsNotNone(self.l.begin("k1:r1", BODY, exempt_attempt=first))

    def test_legacy_ignores_exemption(self):
        legacy = HostedLedger.open_existing(self.path, self.lid, max_outstanding=1)
        first = legacy.begin("k1", BODY)
        with self.assertRaises(HostedAdmissionBlocked):
            legacy.begin("k1:r1", BODY, exempt_attempt=first)
        self.assertEqual(legacy.pending(), [first])             # no phases table: legacy code never adds one


class EndToEnd(Base):
    def setUp(self):
        super().setUp()
        self.provider = Provider(["hang", "ok"])
        self.addCleanup(self.provider.close)
        reset_generation_limiter_for_tests(GenerationLimiter(5, 0))
        s.set_generate_impl_for_tests(None)
        for target, value in (("HOSTED_LEDGER", self.l),
                              ("CHUTES_CONFIG", ChutesConfig(True, "fixture/model", "synthetic-secret")),
                              ("GEN_OPENAI_URL", "http://127.0.0.1:%d/v1" % self.provider.server.server_port),
                              ("GEN_OPENAI_TIMEOUT", 60), ("GEN_ATTEMPT_TIMEOUT", 0.5)):
            p = patch.object(s, target, value)
            p.start()
            self.addCleanup(p.stop)
        self.unknown("c-0"), self.unknown("c-1")                # two fresh unknowns already carried

    def handle(self, key="k1"):
        h = object.__new__(s.Handler)
        h.path = "/generate"
        h.read_json = lambda: {"query": "synthetic question", "contexts": [], "deadline_ms": 20000,
                               "request_key": key}
        out, logs = [], io.StringIO()
        h.write_json = lambda status, body: out.append((status, body))
        with patch.object(s, "client_should_abort", side_effect=lambda handler: False), \
                contextlib.redirect_stdout(logs):
            h.do_POST()
        self.assertEqual(self.l._owned_attempts(), frozenset())
        self.assertEqual(s._HOSTED_IN_FLIGHT, set())
        return out[0], self.retries(logs.getvalue())

    @staticmethod
    def retries(text):
        events = [json.loads(line) for line in text.splitlines() if "generation_retry" in line]
        return [(e["kind"], e["attempt"], e["reason"]) for e in events]

    def test_generate_with_retries_stalled_first_then_success(self):
        budget = s._RetryBudget(time.monotonic() + 20)
        logs = io.StringIO()
        with contextlib.redirect_stdout(logs):
            result = s.generate_with_retries([{"role": "user", "content": "synthetic"}], None, 0, budget=budget,
                                             deadline_at=budget.deadline, request_key="k1")
        self.assertEqual(result, ("final answer", 2, 3))
        self.assertEqual(self.retries(logs.getvalue()), [("post_dispatch", 1, "TimeoutError")])
        self.assertEqual(self.rows(), [("pending", "c-0", "unknown"), ("pending", "c-1", "unknown"),
                                       ("pending", "k1", "unknown"), ("complete", "k1:r1", "terminal")])
        self.assertEqual(len(self.provider.bodies), 2)          # exposure: two provider calls
        self.refused("fresh")                                   # U=3 holds every later admission

    def test_client_gets_200_then_next_request_refused(self):
        (status, body), retries = self.handle()
        self.assertEqual(status, 200, body)
        self.assertIn("answer", body)
        self.assertEqual(retries, [("post_dispatch", 1, "TimeoutError")])
        self.assertEqual([r[:2] for r in self.rows()][-2:], [("pending", "k1"), ("complete", "k1:r1")])
        (status, body), retries = self.handle("k2")
        self.assertEqual((status, body["error"]), (409, "hosted_admission_blocked"))
        self.assertEqual(len(self.provider.bodies), 2)

    def test_without_exemption_the_retry_is_refused(self):
        real = self.l.begin
        with patch.object(self.l, "begin", lambda key=None, digest=None, **kw: real(key, digest)):
            (status, body), retries = self.handle()
        self.assertEqual((status, body["error"]), (504, "generation_backend_timeout"))   # v2p4 outcome
        self.assertEqual(retries, [("post_dispatch", 1, "TimeoutError")])
        self.assertEqual(len(self.provider.bodies), 1)
        self.assertEqual([r[:2] for r in self.rows()][-1], ("pending", "k1"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
