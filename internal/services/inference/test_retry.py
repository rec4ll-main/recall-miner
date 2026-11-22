"""v2p4: bounded generation retries through the real /generate handler (synthetic, offline).

A local plain-HTTP stub on 127.0.0.1 stands in for the provider; no network, no model, fixture
keys only. Pre-dispatch failures spend nothing and are retried with backoff; a post-dispatch
timeout or drop leaves its attempt pending and is retried at most once under key + ":r1".
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
from hosted_accounting import HostedLedger, HostedSlotsBusy  # noqa: E402
from hosted_provider import ChutesConfig  # noqa: E402

OUTPUT = json.dumps({"model": "fixture/model", "choices": [{"finish_reason": "stop", "message": {
    "content": "final answer"}}], "usage": {"prompt_tokens": 2, "completion_tokens": 3}}).encode()


class Provider:
    """Scripted stub: each POST takes the next behaviour ('ok', 'hang' or 'drop')."""

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
                if kind == "drop":
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


class Retry(unittest.TestCase):
    def setUp(self, n=5, script=()):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.path = tmp.name + "/ledger.sqlite"
        self.lid = HostedLedger.initialize(self.path)
        self.ledger = HostedLedger.open_existing(self.path, self.lid, max_outstanding=2,
                                                 recovery_policy="carried-unknown-v1", max_concurrent=n)
        self.provider = Provider(script)
        self.addCleanup(self.provider.close)
        self.limiter = GenerationLimiter(5, 0)
        reset_generation_limiter_for_tests(self.limiter)
        s.set_generate_impl_for_tests(None)
        for target, value in (("HOSTED_LEDGER", self.ledger),
                              ("CHUTES_CONFIG", ChutesConfig(True, "fixture/model", "synthetic-secret")),
                              ("GEN_OPENAI_URL", "http://127.0.0.1:%d/v1" % self.provider.server.server_port),
                              ("GEN_OPENAI_TIMEOUT", 60), ("GEN_ATTEMPT_TIMEOUT", 0.5)):
            p = patch.object(s, target, value)
            p.start()
            self.addCleanup(p.stop)

    def reopen(self, n, script=()):
        self.doCleanups()
        self.setUp(n=n, script=script)

    def no_attempt_timeout_expected(self):
        # setUp's 0.5 s attempt budget lets the "hang" scripts time out quickly. A test in which
        # no attempt may time out must not depend on it: an attempt that succeeds still spends its
        # budget on the ledger (BEGIN IMMEDIATE, synchronous=FULL commits, pin checks) before the
        # request is sent, and one such begin took over 1 s on a loaded CI runner, so the attempt
        # was reserved and then refused with 504 generation_backend_timeout. The request deadline
        # (20 s) and the retry rules are unchanged.
        p = patch.object(s, "GEN_ATTEMPT_TIMEOUT", 10.0)
        p.start()
        self.addCleanup(p.stop)

    def handle(self, payload, abort=lambda: False):
        h = object.__new__(s.Handler)
        h.path = "/generate"
        h.read_json = lambda: payload
        out, logs = [], io.StringIO()
        h.write_json = lambda status, body: out.append((status, body))
        before = self.ledger._owned_attempts()
        with patch.object(s, "client_should_abort", side_effect=lambda handler: abort()), \
                contextlib.redirect_stdout(logs):
            h.do_POST()
        self.assertLessEqual(self.ledger._owned_attempts(), before)   # the request's slots released
        self.assertEqual(s._HOSTED_IN_FLIGHT, set())
        return out[0], self.retries(logs.getvalue())

    def retries(self, text):
        events = [json.loads(line) for line in text.splitlines() if "generation_retry" in line]
        for e in events:
            self.assertEqual(set(e), {"event", "kind", "attempt", "reason"})   # no keys or bodies
        return [(e["kind"], e["attempt"], e["reason"]) for e in events]

    def rows(self):
        with contextlib.closing(sqlite3.connect("file:%s?mode=ro" % self.path, uri=True)) as c:
            return c.execute("SELECT a.state, r.request_key, r.sent, p.phase FROM hosted_attempts a "
                             "JOIN hosted_attempt_requests r USING(id) LEFT JOIN hosted_attempt_phases p "
                             "USING(id) ORDER BY a.rowid").fetchall()

    def request(self, key="k1", deadline_ms=20000):
        payload = {"query": "synthetic question", "contexts": [], "deadline_ms": deadline_ms}
        if key is not None:
            payload["request_key"] = key
        return payload

    def test_pre_dispatch_connect_failure_then_success(self):
        self.no_attempt_timeout_expected()
        real, calls = s.http.client.HTTPConnection.connect, []

        def flaky(conn):
            calls.append(1)
            if len(calls) == 1:
                raise ConnectionRefusedError("synthetic")
            return real(conn)
        with patch.object(s.http.client.HTTPConnection, "connect", flaky):
            (status, body), retries = self.handle(self.request())
        self.assertEqual(status, 200, body)
        self.assertEqual(retries, [("pre_dispatch", 1, "HostedConnectFailed")])
        self.assertEqual(self.rows(), [("complete", "k1", 1, "terminal")])   # one ledger attempt
        self.assertEqual(len(self.provider.bodies), 1)

    def test_post_dispatch_timeout_then_success_uses_new_key(self):
        self.reopen(5, ["hang", "ok"])
        (status, body), retries = self.handle(self.request())
        self.assertEqual(status, 200, body)
        self.assertEqual(retries, [("post_dispatch", 1, "TimeoutError")])
        self.assertEqual(self.rows(), [("pending", "k1", 1, "unknown"),        # first stays pending
                                       ("complete", "k1:r1", 1, "terminal")])
        self.assertEqual(len(self.provider.bodies), 2)
        self.assertEqual(self.provider.bodies[0], self.provider.bodies[1])

    def test_post_dispatch_drop_then_success(self):
        self.reopen(5, ["drop", "ok"])
        (status, body), retries = self.handle(self.request())
        self.assertEqual(status, 200, body)
        self.assertEqual(retries, [("post_dispatch", 1, "GenerationCanceled")])
        self.assertEqual([r[:2] for r in self.rows()], [("pending", "k1"), ("complete", "k1:r1")])

    def test_post_dispatch_retried_at_most_once(self):
        self.reopen(5, ["hang", "hang", "ok"])
        (status, body), retries = self.handle(self.request(deadline_ms=30000))
        self.assertEqual((status, body["error"]), (504, "generation_backend_timeout"))
        self.assertEqual(retries, [("post_dispatch", 1, "TimeoutError")])
        self.assertEqual(self.rows(), [("pending", "k1", 1, "unknown"), ("pending", "k1:r1", 1, "unknown")])
        self.assertEqual(len(self.provider.bodies), 2)

    def test_post_dispatch_timeout_without_15s_left_is_not_retried(self):
        self.reopen(5, ["hang", "ok"])
        (status, body), retries = self.handle(self.request(deadline_ms=10000))
        self.assertEqual((status, body["error"]), (504, "generation_backend_timeout"))
        self.assertEqual(retries, [])
        self.assertEqual(self.rows(), [("pending", "k1", 1, "unknown")])
        self.assertEqual(len(self.provider.bodies), 1)

    def test_existing_key_conflict_is_never_retried(self):
        prior = self.ledger.begin("k1", "0" * 64)
        self.ledger.complete(prior)
        self.ledger.finish_local(prior)
        (status, body), retries = self.handle(self.request())
        self.assertEqual((status, body["error"]), (409, "duplicate_request_key"))
        self.assertEqual(retries, [])
        self.assertEqual(self.provider.bodies, [])
        self.assertEqual(len(self.rows()), 1)

    def test_used_retry_key_refuses_the_retry_and_reports_the_timeout(self):
        self.reopen(5, ["hang", "ok"])
        prior = self.ledger.begin("k1:r1", "0" * 64)
        self.ledger.complete(prior)
        self.ledger.finish_local(prior)
        (status, body), retries = self.handle(self.request())
        self.assertEqual((status, body["error"]), (504, "generation_backend_timeout"))
        self.assertEqual(retries, [("post_dispatch", 1, "TimeoutError")])   # tried, refused at begin
        self.assertEqual(len(self.provider.bodies), 1)                     # nothing dispatched twice
        self.assertEqual([r[:2] for r in self.rows()], [("complete", "k1:r1"), ("pending", "k1")])

    def test_key_near_length_limit_is_not_truncated_or_retried(self):
        self.reopen(5, ["hang", "ok"])
        key = "k" * 126
        (status, body), retries = self.handle(self.request(key=key))
        self.assertEqual((status, body["error"]), (504, "generation_backend_timeout"))
        self.assertEqual(retries, [])
        self.assertEqual(len(self.provider.bodies), 1)

    def test_keyless_request_is_not_retried_after_dispatch(self):
        # Recovery policies require a key, so a keyless request is refused before dispatch
        # (unchanged); it is never retried as post-dispatch.
        self.reopen(5, ["hang", "ok"])
        (status, _), retries = self.handle(self.request(key=None))
        self.assertEqual(status, 409)
        self.assertEqual(retries, [])
        self.assertEqual(self.provider.bodies, [])

    def test_client_abort_after_dispatch_waits_for_the_attempt_and_is_not_retried(self):
        # A sent attempt is no longer abandoned on a client abort: it runs to its own limit
        # (here the stalled attempt's timeout); the gone client then gets no retry.
        self.reopen(5, ["hang", "ok"])
        (status, body), retries = self.handle(self.request(), abort=lambda: bool(self.provider.bodies))
        self.assertEqual((status, body["error"]), (504, "generation_backend_timeout"))
        self.assertEqual(retries, [])
        self.assertEqual(len(self.provider.bodies), 1)

    def test_all_slots_busy_at_admission_then_free(self):
        self.reopen(1)
        self.no_attempt_timeout_expected()
        holder = self.ledger.begin("holder", "0" * 64)

        def free():
            self.ledger.complete(holder)
            self.ledger.finish_local(holder)
        threading.Timer(0.3, free).start()
        (status, body), retries = self.handle(self.request())
        self.assertEqual(status, 200, body)
        self.assertTrue(retries)
        self.assertTrue(all(r[0] == "pre_dispatch" and r[2] == "HostedSlotsBusy" for r in retries))
        self.assertEqual([r[:2] for r in self.rows()], [("complete", "holder"), ("complete", "k1")])

    def test_all_slots_busy_at_begin_then_free(self):
        self.reopen(1)
        self.no_attempt_timeout_expected()
        holder = self.ledger.begin("holder", "0" * 64)

        def free():
            self.ledger.complete(holder)
            self.ledger.finish_local(holder)
        threading.Timer(1.2, free).start()                # outlasts begin's 1 s slot wait
        budget = s._RetryBudget(time.monotonic() + 20)
        logs = io.StringIO()
        with contextlib.redirect_stdout(logs):
            result = s.generate_with_retries([{"role": "user", "content": "synthetic"}], None, 0, budget=budget,
                                             deadline_at=budget.deadline, request_key="k1")
        self.assertEqual(result, ("final answer", 2, 3))
        self.assertEqual(self.retries(logs.getvalue()), [("pre_dispatch", 1, "HostedSlotsBusy")])
        self.assertEqual([r[:2] for r in self.rows()], [("complete", "holder"), ("complete", "k1")])

    def test_slots_busy_until_deadline_refuses(self):
        self.reopen(1)
        holder = self.ledger.begin("holder", "0" * 64)
        (status, body), retries = self.handle(self.request(deadline_ms=6000))
        self.assertEqual((status, body["error"]), (409, "hosted_admission_blocked"))
        self.assertTrue(retries)
        self.assertLessEqual(len(retries), 2)              # stops once a pause would leave < 5 s
        self.assertEqual(self.provider.bodies, [])
        self.ledger.finish_local(holder)

    def test_limiter_overloaded_then_free(self):
        self.no_attempt_timeout_expected()
        limiter = GenerationLimiter(1, 0)
        reset_generation_limiter_for_tests(limiter)
        limiter.acquire()
        threading.Timer(0.3, limiter.release).start()
        (status, body), retries = self.handle(self.request())
        self.assertEqual(status, 200, body)
        self.assertTrue(retries)
        self.assertTrue(all(r[:3:2] == ("pre_dispatch", "GenerationOverloaded") for r in retries))

    def test_attempt_timeout_validation(self):
        with patch.object(s, "GEN_ATTEMPT_TIMEOUT", float("nan")):
            self.assertEqual(s.generation_readiness(), (False, "hosted deadline unavailable"))
            (status, _), _ = self.handle(self.request())
        self.assertEqual(status, 409)
        with patch.object(s, "GEN_ATTEMPT_TIMEOUT", None):
            self.assertTrue(s.generation_readiness()[0])


if __name__ == "__main__":
    unittest.main(verbosity=2)
