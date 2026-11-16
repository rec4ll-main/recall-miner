"""A complete 4xx/5xx provider response is terminal (owner decision 2026-11-06).

During a Chutes outage every call came back as a complete 502 within ~70 ms. Those used to stay
pending ("may still be running"), so two of them held admission for an hour and the miner stopped
advertising generation. A complete 5xx is now recorded terminal as provider_rejected with its
HTTP status: the failed call fails, admission stays open, and nothing is retried. A 408 still
stays pending. Synthetic and offline: a local stub on 127.0.0.1 stands in for the provider.
"""
import contextlib
import io
import os
import sqlite3
import sys
import tempfile
import threading
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
from hosted_accounting import AGING_POLICY, HostedLedger  # noqa: E402
from hosted_provider import ChutesConfig  # noqa: E402

BODY = b'{"detail":"no instances available"}'


class StatusProvider:
    """Answers every POST with a complete response carrying the given status."""

    def __init__(self, status):
        self.calls = 0
        provider = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                provider.calls += 1
                self.close_connection = True
                self.connection.sendall(b"HTTP/1.1 %d X\r\nContent-Length: %d\r\nConnection: close\r\n\r\n"
                                        % (status, len(BODY)) + BODY)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class ProviderHTTPFailure(unittest.TestCase):
    def start(self, status):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.path = tmp.name + "/ledger.sqlite"
        lid = HostedLedger.initialize(self.path)
        # The production configuration: aging policy, bound two, four active attempts.
        self.ledger = HostedLedger.open_existing(self.path, lid, max_outstanding=2, recovery_policy=AGING_POLICY,
                                                 unknown_age_out_seconds=3600, max_concurrent=4)
        self.provider = StatusProvider(status)
        self.addCleanup(self.provider.close)
        reset_generation_limiter_for_tests(GenerationLimiter(4, 8))
        s.set_generate_impl_for_tests(None)
        for target, value in (("HOSTED_LEDGER", self.ledger), ("HOSTED_LEDGER_ERROR", ""),
                              ("CHUTES_CONFIG", ChutesConfig(True, "fixture/model", "synthetic-secret")),
                              ("GEN_OPENAI_URL", "http://127.0.0.1:%d/v1" % self.provider.server.server_port),
                              ("GEN_OPENAI_TIMEOUT", 60), ("GEN_ATTEMPT_TIMEOUT", 10.0)):
            p = patch.object(s, target, value)
            p.start()
            self.addCleanup(p.stop)

    def generate(self, key):
        h = object.__new__(s.Handler)
        h.path = "/generate"
        h.read_json = lambda: {"query": "synthetic question", "contexts": [], "deadline_ms": 20000,
                               "request_key": key}
        out = []
        h.write_json = lambda status, body: out.append((status, body))
        with patch.object(s, "client_should_abort", return_value=False), contextlib.redirect_stdout(io.StringIO()):
            h.do_POST()
        self.assertEqual(s._HOSTED_IN_FLIGHT, set())
        return out[0]

    def rows(self):
        with contextlib.closing(sqlite3.connect("file:%s?mode=ro" % self.path, uri=True)) as c:
            return c.execute("SELECT a.state, o.outcome, o.http_status FROM hosted_attempts a "
                             "LEFT JOIN hosted_attempt_outcomes o USING(id) ORDER BY a.rowid").fetchall()

    def test_complete_5xx_is_terminal_and_keeps_generation_ready(self):
        for status in (500, 502, 503, 504):
            with self.subTest(status=status):
                self.start(status)
                for i in range(5):   # more failures than the two carried unknowns ever allowed
                    code, body = self.generate("k%d-%d" % (status, i))
                    self.assertGreaterEqual(code, 500)
                    self.assertNotIn("no instances available", str(body))   # provider body never echoed
                self.assertEqual(self.provider.calls, 5)                    # one call each, no retries
                self.assertEqual(self.rows(), [("complete", "provider_rejected", status)] * 5)
                self.assertEqual(s.generation_readiness(), (True, ""))
                self.doCleanups()

    def test_complete_4xx_unchanged(self):
        self.start(400)
        self.generate("k400")
        self.assertEqual(self.rows(), [("complete", "provider_rejected", 400)])
        self.assertEqual(s.generation_readiness(), (True, ""))

    def test_408_stays_pending(self):
        self.start(408)
        self.generate("k408-a")
        self.generate("k408-b")
        self.generate("k408-c")
        states = [r[0] for r in self.rows()]
        self.assertTrue(states and all(st == "pending" for st in states), states)
        self.assertEqual(s.generation_readiness(), (False, "unresolved hosted attempt held"))


if __name__ == "__main__":
    unittest.main()
