"""Bounded admission, durable request-key deduplication and the sent marker (offline candidate).

Separately versioned recovery candidate for the generation holds. Nothing here clears,
completes, expires or replays a pending attempt. The default bound (1) is the accepted
refusal: no new dispatch while any attempt is pending. A larger bound is proposed policy
only; it admits distinct new attempts while older ones stay pending and counted, and it
proves nothing about whether their remote work ended. Synthetic disposable ledgers only.
"""

import contextlib
import hashlib
import json
import os
import signal
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

from hosted_accounting import HostedAdmissionBlocked, HostedLedger, HostedRequestKeyConflict

HERE = os.path.dirname(os.path.abspath(__file__))


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = tmp.name
        self.path = os.path.join(self.dir, "ledger.sqlite")
        self.ledger_id = HostedLedger.initialize(self.path)

    def open(self, bound=1):
        return HostedLedger.open_existing(self.path, self.ledger_id, max_outstanding=bound)

    def child(self, code, *args, wait=True):
        script = ("import sys, json, os, signal\nsys.path.insert(0, %r)\n"
                  "from hosted_accounting import HostedLedger, HostedAdmissionBlocked\n"
                  "path, lid = sys.argv[1], sys.argv[2]\n" % HERE) + code
        p = subprocess.Popen([sys.executable, "-c", script, self.path, self.ledger_id, *map(str, args)],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        if not wait:
            return p
        out, err = p.communicate(timeout=60)
        return p.returncode, out, err

    def pending_rows(self):
        conn = sqlite3.connect(self.path)
        try:
            return conn.execute("SELECT count(*) FROM hosted_attempts WHERE state='pending'").fetchone()[0]
        finally:
            conn.close()


class DefaultBoundTests(Base):
    def test_default_refuses_any_pending_attempt(self):
        led = self.open()
        self.assertEqual(led.max_outstanding, 1)
        led.begin()
        with self.assertRaises(HostedAdmissionBlocked):
            led.begin()
        self.assertFalse(led.available())

    def test_legacy_pending_row_counts_and_reads_as_possibly_sent(self):
        conn = sqlite3.connect(self.path)
        conn.execute("INSERT INTO hosted_attempts(id, state) VALUES ('legacy-1', 'pending')")
        conn.commit()
        conn.close()
        led = self.open(bound=2)
        self.assertEqual(led.pending_detail(), [{"id": "legacy-1", "sent": None}])
        led.begin()                                  # 1 legacy + 1 new = 2 = bound
        with self.assertRaises(HostedAdmissionBlocked):
            led.begin()
        self.assertEqual(self.pending_rows(), 2)

    def test_invalid_bounds_refused(self):
        for bad in (0, -1, True, "2", 1.5, None):
            with self.subTest(bound=bad), self.assertRaises(HostedAdmissionBlocked):
                HostedLedger.open_existing(self.path, self.ledger_id, max_outstanding=bad)


class SameProcessTimeoutTests(Base):
    def test_timed_out_attempt_stays_counted_without_restart(self):
        led = self.open(bound=2)
        a = led.begin()
        led.mark_sent(a)                              # sent, then timed out: no terminal recorded
        self.assertTrue(led.available(in_flight={a}))  # while its own request runs
        self.assertTrue(led.available())               # after it left in-flight: 1 unresolved < 2
        b = led.begin()                                # admitted: 2 pending = bound
        # Readiness excludes the caller's own running request (b), so it stays ready, but no
        # further attempt can be reserved while a (unresolved) and b (running) are both pending.
        self.assertTrue(led.available(in_flight={b}))
        with self.assertRaises(HostedAdmissionBlocked):
            led.begin()
        self.assertFalse(led.available())               # once b is also unresolved: 2 >= bound
        self.assertEqual({d["id"]: d["sent"] for d in led.pending_detail()}, {a: True, b: False})
        self.assertEqual(self.pending_rows(), 2)       # nothing cleared or completed


class BoundTests(Base):
    def test_pending_never_exceeds_the_bound(self):
        for bound in (1, 2, 3):
            with self.subTest(bound=bound):
                self.setUp()
                led = self.open(bound)
                for _ in range(bound):
                    led.begin()
                with self.assertRaises(HostedAdmissionBlocked):
                    led.begin()
                self.assertEqual(self.pending_rows(), bound)

    def test_simultaneous_admission_at_the_bound_admits_exactly_one(self):
        self.open(bound=2).begin()                     # one unresolved already
        ledgers = [self.open(bound=2) for _ in range(8)]  # separate objects: separate in-process locks
        barrier, results = threading.Barrier(8), []

        def go(led):
            barrier.wait()
            try:
                results.append(led.begin())
            except HostedAdmissionBlocked:
                results.append(None)

        threads = [threading.Thread(target=go, args=(led,)) for led in ledgers]
        [t.start() for t in threads]
        [t.join(30) for t in threads]
        self.assertEqual(sum(r is not None for r in results), 1)
        self.assertEqual(self.pending_rows(), 2)

    def test_multiple_processes_respect_the_bound(self):
        go = os.path.join(self.dir, "go")
        code = ("import time\nwhile not os.path.exists(%r): time.sleep(0.005)\n"
                "led = HostedLedger.open_existing(path, lid, max_outstanding=2)\n"
                "try:\n    print(led.begin())\nexcept HostedAdmissionBlocked:\n    print('refused')\n" % go)
        procs = [self.child(code, wait=False) for _ in range(6)]
        open(go, "w").close()
        outs = [p.communicate(timeout=60)[0].strip() for p in procs]
        self.assertEqual(sum(o != "refused" for o in outs), 2, outs)
        self.assertEqual(self.pending_rows(), 2)


class RequestKeyTests(Base):
    def test_duplicate_key_refused_in_process_and_after_completion(self):
        led = self.open(bound=3)
        a = led.begin("req-1")
        with self.assertRaises(HostedAdmissionBlocked):
            led.begin("req-1")
        led.mark_sent(a)
        led.complete(a, usage=(1, 1))
        with self.assertRaises(HostedAdmissionBlocked):  # never re-dispatched, even once resolved
            led.begin("req-1")
        led.begin("req-2")                               # a distinct key is admitted

    def test_duplicate_key_refused_across_processes_and_restart(self):
        rc, out, err = self.child("led = HostedLedger.open_existing(path, lid, max_outstanding=3)\nprint(led.begin('req-x'))\n")
        self.assertEqual(rc, 0, err)
        rc, out, err = self.child("led = HostedLedger.open_existing(path, lid, max_outstanding=3)\n"
                                  "try:\n    led.begin('req-x'); print('admitted')\n"
                                  "except HostedAdmissionBlocked as e:\n    print('refused')\n")
        self.assertEqual(out.strip(), "refused")
        with self.assertRaises(HostedAdmissionBlocked):
            self.open(bound=3).begin("req-x")             # fresh object = restart

    def test_concurrent_duplicate_keys_admit_exactly_one(self):
        go = os.path.join(self.dir, "go")
        code = ("import time\nwhile not os.path.exists(%r): time.sleep(0.005)\n"
                "led = HostedLedger.open_existing(path, lid, max_outstanding=10)\n"
                "try:\n    print(led.begin('same-key'))\nexcept HostedAdmissionBlocked:\n    print('refused')\n" % go)
        procs = [self.child(code, wait=False) for _ in range(5)]
        open(go, "w").close()
        outs = [p.communicate(timeout=60)[0].strip() for p in procs]
        self.assertEqual(sum(o != "refused" for o in outs), 1, outs)

    def test_invalid_keys_refused(self):
        led = self.open(bound=3)
        for bad in ("", 7, "x" * 257):
            with self.subTest(key=bad), self.assertRaises(HostedAdmissionBlocked):
                led.begin(bad)
        self.assertEqual(self.pending_rows(), 0)


D1, D2 = hashlib.sha256(b"body-1").hexdigest(), hashlib.sha256(b"body-2").hexdigest()


class RequestBindingTests(Base):
    """A key is bound durably to the digest of the body it first reserved."""

    def test_same_key_same_body_refused_as_duplicate(self):
        led = self.open(bound=3)
        led.begin("k", D1)
        with self.assertRaisesRegex(HostedRequestKeyConflict, "already used"):
            led.begin("k", D1)
        self.assertEqual(self.pending_rows(), 1)

    def test_same_key_different_body_refused_as_mismatch(self):
        led = self.open(bound=3)
        led.begin("k", D1)
        with self.assertRaisesRegex(HostedRequestKeyConflict, "different body"):
            led.begin("k", D2)
        self.assertEqual(self.pending_rows(), 1)

    def test_conflicts_are_admission_refusals(self):
        self.assertTrue(issubclass(HostedRequestKeyConflict, HostedAdmissionBlocked))

    def test_mismatch_and_duplicate_refused_after_every_terminal_outcome_and_restart(self):
        from hosted_accounting import OUTCOMES
        led = self.open(bound=3)
        for i, outcome in enumerate(sorted(OUTCOMES)):
            with self.subTest(outcome=outcome):
                a = led.begin("t%d" % i, D1)
                led.mark_sent(a)
                led.complete(a, outcome)
                fresh = self.open(bound=3)              # restart
                for digest, why in ((D1, "already used"), (D2, "different body")):
                    with self.assertRaisesRegex(HostedRequestKeyConflict, why):
                        fresh.begin("t%d" % i, digest)
        self.assertEqual(self.pending_rows(), 0)

    def test_concurrent_same_key_mixed_bodies_admit_exactly_one(self):
        go = os.path.join(self.dir, "go")
        code = ("import time\nwhile not os.path.exists(%r): time.sleep(0.005)\n"
                "led = HostedLedger.open_existing(path, lid, max_outstanding=10)\n"
                "try:\n    print(led.begin('shared', sys.argv[3]))\n"
                "except HostedAdmissionBlocked as e:\n    print('refused:' + str(e))\n" % go)
        procs = [self.child(code, D1 if i % 2 else D2, wait=False) for i in range(6)]
        open(go, "w").close()
        outs = [p.communicate(timeout=60)[0].strip() for p in procs]
        self.assertEqual(sum(not o.startswith("refused") for o in outs), 1, outs)
        self.assertEqual(self.pending_rows(), 1)

    def test_digest_requires_a_key_and_sha256_form(self):
        led = self.open(bound=3)
        for key, digest in ((None, D1), ("k", "ABC"), ("k", D1.upper()), ("k", D1[:-1]), ("k", 5)):
            with self.subTest(key=key, digest=digest), self.assertRaises(HostedAdmissionBlocked):
                led.begin(key, digest)
        self.assertEqual(self.pending_rows(), 0)

    def test_key_without_digest_still_never_redispatched(self):
        led = self.open(bound=3)
        led.begin("bare")
        for digest in (None, D1):
            with self.assertRaisesRegex(HostedRequestKeyConflict, "already used"):
                led.begin("bare", digest)

    def test_held_candidate_table_gains_the_binding_column(self):
        conn = sqlite3.connect(self.path)
        conn.execute("CREATE TABLE hosted_attempt_requests (id TEXT PRIMARY KEY REFERENCES hosted_attempts(id), "
                     "request_key TEXT UNIQUE, sent INTEGER NOT NULL DEFAULT 0 CHECK(sent IN (0,1)))")
        conn.commit()
        conn.close()
        led = self.open(bound=3)
        led.begin("k", D1)
        with self.assertRaisesRegex(HostedRequestKeyConflict, "different body"):
            led.begin("k", D2)


class CrashAndCommitTests(Base):
    def kill_after(self, step):
        code = ("led = HostedLedger.open_existing(path, lid, max_outstanding=3)\n"
                "a = led.begin('crash-key')\n" + ("led.mark_sent(a)\n" if step == "sent" else "") +
                "print(a, flush=True)\nos.kill(os.getpid(), signal.SIGKILL)\n")
        rc, out, _ = self.child(code)
        self.assertEqual(rc, -signal.SIGKILL)
        return out.strip()

    def test_crash_after_reservation_is_pending_and_unsent(self):
        a = self.kill_after("reserved")
        led = self.open(bound=3)
        self.assertEqual(led.pending_detail(), [{"id": a, "sent": False}])
        with self.assertRaises(HostedAdmissionBlocked):   # the key is still used: no automatic replay
            led.begin("crash-key")

    def test_crash_after_sent_marker_is_pending_and_possibly_sent(self):
        a = self.kill_after("sent")
        self.assertEqual(self.open(bound=3).pending_detail(), [{"id": a, "sent": True}])
        self.assertEqual(self.pending_rows(), 1)

    def _failing_commit(self, led, on_write_number):
        original, counter = led.connect, {"n": 0}

        @contextlib.contextmanager
        def failing(*args, **kwargs):
            with original(*args, **kwargs) as conn:
                yield conn
                if kwargs.get("write"):
                    counter["n"] += 1
                    if counter["n"] == on_write_number:
                        raise OSError("synthetic commit failure")
        led.connect = failing
        return original

    def test_uncertain_reservation_commit_leaves_nothing_reserved(self):
        led = self.open(bound=2)
        self._failing_commit(led, 1)
        with self.assertRaises(OSError):
            led.begin("res-key")
        self.assertEqual(self.pending_rows(), 0)          # rolled back: nothing to send
        led2 = self.open(bound=2)
        led2.begin("res-key")                             # never sent, so the key was never used

    def test_uncertain_sent_marker_commit_means_do_not_send(self):
        led = self.open(bound=2)
        a = led.begin("sent-key")
        original = self._failing_commit(led, 1)
        with self.assertRaises(OSError):
            led.mark_sent(a)
        led.connect = original
        self.assertEqual(led.pending_detail(), [{"id": a, "sent": False}])  # still pending and counted
        with self.assertRaises(HostedAdmissionBlocked):
            led.begin("sent-key")

    def test_uncertain_terminal_commit_after_send_stays_possibly_sent(self):
        led = self.open(bound=2)
        a = led.begin("term-key")
        led.mark_sent(a)
        original = self._failing_commit(led, 1)
        with self.assertRaises(OSError):
            led.complete(a, usage=(1, 1))
        led.connect = original
        self.assertEqual(led.pending_detail(), [{"id": a, "sent": True}])   # never described as unsent

    def test_sent_marker_only_for_a_pending_unsent_attempt(self):
        led = self.open(bound=2)
        a = led.begin()
        led.mark_sent(a)
        with self.assertRaises(HostedAdmissionBlocked):
            led.mark_sent(a)                              # already marked
        led.complete(a, usage=(1, 1))
        b = led.begin()
        led.complete(b, "provider_rejected", http_status=400)
        with self.assertRaises(HostedAdmissionBlocked):
            led.mark_sent(b)                              # terminal: must not be "sent" afterwards
        with self.assertRaises(HostedAdmissionBlocked):
            led.mark_sent("no-such-attempt")

    def test_reads_do_not_create_the_new_table(self):
        with open(self.path, "rb") as f:
            before = f.read()
        led = self.open(bound=2)
        led.available()
        led.pending_detail()
        led.pending()
        HostedLedger.status(self.path, self.ledger_id)
        with open(self.path, "rb") as f:
            self.assertEqual(f.read(), before)


class ServiceFixture(unittest.TestCase):
    """Synthetic enabled Chutes behind a loopback backend that accepts the POST and replies malformed."""

    def setUp(self):
        import types
        sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
        sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
        import service
        from chutes_fixture import SyntheticChutes
        self.service, self.posts = service, []
        outer = self

        class Malformed(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                outer.posts.append(1)
                self.send_response(200)
                self.send_header("Content-Length", "9")
                self.end_headers()
                self.wfile.write(b'{"choices')

            def log_message(self, *a):
                return

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), Malformed)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)
        self.chutes = SyntheticChutes(self, self.srv.server_address[1])


class ServiceSentMarkerTests(ServiceFixture):
    """The sidecar's real dispatch path records sent=1 only for attempts it actually dispatched."""

    def test_abort_before_any_reservation_reserves_nothing(self):
        with self.assertRaises(self.service.GenerationCanceled):
            self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0, should_abort=lambda: True)
        self.assertEqual((self.posts, self.chutes.ledger.pending_detail()), ([], []))

    def test_abort_after_reservation_before_send_is_recorded_unsent(self):
        calls = []

        def abort_on_second_check():  # first check precedes begin(); the second follows it
            calls.append(1)
            return len(calls) >= 2

        with self.assertRaises(self.service.GenerationCanceled):
            self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0, should_abort=abort_on_second_check)
        self.assertEqual(self.posts, [])
        self.assertEqual([d["sent"] for d in self.chutes.ledger.pending_detail()], [False])  # held, provably unsent

    def _no_request(self):
        calls = []
        real = self.service.http.client.HTTPConnection.request

        def record(conn, *a, **kw):
            calls.append(1)
            return real(conn, *a, **kw)

        return calls, mock.patch.object(self.service.http.client.HTTPConnection, "request", record)

    def test_cancel_during_marker_commit_sends_nothing_and_keeps_the_row(self):
        flag = []
        real = self.chutes.ledger.mark_sent
        calls, patch_request = self._no_request()
        with mock.patch.object(self.chutes.ledger, "mark_sent", lambda a: (real(a), flag.append(1))), patch_request:
            with self.assertRaises(self.service.GenerationCanceled):
                self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0, should_abort=lambda: bool(flag))
        self.assertEqual((calls, self.posts), ([], []))
        self.assertEqual([d["sent"] for d in self.chutes.ledger.pending_detail()], [True])  # held, possibly sent

    def test_deadline_expiry_during_marker_commit_sends_nothing_and_keeps_the_row(self):
        deadline = time.monotonic() + 0.4
        real = self.chutes.ledger.mark_sent

        def slow_commit(attempt):
            real(attempt)
            time.sleep(max(0.0, deadline - time.monotonic()) + 0.05)

        calls, patch_request = self._no_request()
        with mock.patch.object(self.chutes.ledger, "mark_sent", slow_commit), patch_request:
            with self.assertRaises(TimeoutError):
                self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0, deadline_at=deadline)
        self.assertEqual((calls, self.posts), ([], []))
        self.assertEqual([d["sent"] for d in self.chutes.ledger.pending_detail()], [True])

    def test_marker_committed_but_ack_lost_sends_nothing(self):
        real = self.chutes.ledger.mark_sent

        def lost(attempt):
            real(attempt)
            raise OSError("synthetic acknowledgment lost after commit")

        from hosted_provider import HostedProviderError
        calls, patch_request = self._no_request()
        with mock.patch.object(self.chutes.ledger, "mark_sent", lost), patch_request:
            with self.assertRaises(HostedProviderError):
                self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0)
        self.assertEqual((calls, self.posts), ([], []))
        self.assertEqual([d["sent"] for d in self.chutes.ledger.pending_detail()], [True])

    def test_keyed_replay_is_refused_without_a_second_post(self):
        from hosted_provider import HostedProviderError
        self.chutes.ledger.max_outstanding = 3
        msgs = [{"role": "user", "content": "q"}]
        with self.assertRaises(HostedProviderError):
            self.service.generate_via_openai(msgs, None, 0, request_key="api-req-1")
        with self.assertRaisesRegex(HostedRequestKeyConflict, "already used"):
            self.service.generate_via_openai(msgs, None, 0, request_key="api-req-1")
        with self.assertRaisesRegex(HostedRequestKeyConflict, "different body"):
            self.service.generate_via_openai([{"role": "user", "content": "other"}], None, 0, request_key="api-req-1")
        self.assertEqual(self.posts, [1])
        self.assertEqual(len(self.chutes.ledger.pending_detail()), 1)

    def test_unkeyed_calls_keep_the_old_behaviour(self):
        from hosted_provider import HostedProviderError
        with self.assertRaises(HostedProviderError):
            self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0)
        with self.assertRaisesRegex(HostedAdmissionBlocked, "unresolved"):   # default bound 1 still holds
            self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0)
        self.assertEqual(self.posts, [1])

    def test_dispatched_uncertain_attempt_is_recorded_sent(self):
        from hosted_provider import HostedProviderError
        with self.assertRaises(HostedProviderError):
            self.service.generate_via_openai([{"role": "user", "content": "q"}], None, 0)
        self.assertEqual(self.posts, [1])
        self.assertEqual([d["sent"] for d in self.chutes.ledger.pending_detail()], [True])


class HandlerRequestKeyTests(ServiceFixture):
    """/generate validates the optional request_key and passes it to the hosted dispatch."""

    def post(self, payload):
        from http.server import ThreadingHTTPServer
        srv = ThreadingHTTPServer(("127.0.0.1", 0), self.service.Handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            import http.client as hc
            c = hc.HTTPConnection("127.0.0.1", srv.server_address[1], timeout=20)
            c.request("POST", "/generate", body=json.dumps(payload), headers={"Content-Type": "application/json"})
            r = c.getresponse()
            out = (r.status, json.loads(r.read() or b"{}"))
            c.close()
            return out
        finally:
            srv.shutdown()
            srv.server_close()

    def test_malformed_keys_refused_before_any_reservation(self):
        for bad in ("", "has space", "x" * 129, 7, ["k"], "k/../x"):
            with self.subTest(key=bad):
                self.assertEqual(self.post({"query": "q", "contexts": [], "request_key": bad}),
                                 (400, {"error": "invalid_request_key"}))
        self.assertEqual((self.posts, self.chutes.ledger.pending_detail()), ([], []))

    def test_key_is_passed_through_and_replay_is_409(self):
        seen = []
        real = self.service.generate_via_openai

        def spy(*a, **kw):
            seen.append(kw.get("request_key"))
            return real(*a, **kw)

        self.service._GENERATE_IMPL = None
        self.chutes.ledger.max_outstanding = 3
        with mock.patch.object(self.service, "generate_via_openai", spy):
            first = self.post({"query": "q", "contexts": [], "request_key": "api:req-1"})
            second = self.post({"query": "q", "contexts": [], "request_key": "api:req-1"})
            unkeyed = self.post({"query": "q", "contexts": []})
        self.assertEqual(seen, ["api:req-1", "api:req-1", None])
        self.assertNotEqual(first[0], 200)                          # malformed loopback reply
        self.assertEqual(second[0], 409)
        self.assertEqual(second[1]["error"], "duplicate_request_key")
        self.assertNotEqual(unkeyed[1].get("error"), "duplicate_request_key")
        self.assertEqual(len(self.posts), 2)                        # the replay never reached the provider


class ServiceWiringTests(unittest.TestCase):
    STUB = ('import sys,types\n'
            'sys.modules.setdefault("sentence_transformers",types.SimpleNamespace(CrossEncoder=object,SentenceTransformer=object))\n'
            'sys.modules.setdefault("transformers",types.SimpleNamespace(AutoModelForCausalLM=object,AutoTokenizer=object))\n'
            'import service\n'
            'print(service.HOSTED_LEDGER is None, service.HOSTED_LEDGER and service.HOSTED_LEDGER.max_outstanding, '
            'service.HOSTED_LEDGER_ERROR)\n')

    def start(self, bound):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "l.sqlite")
            lid = HostedLedger.initialize(path)
            key = os.path.join(d, "key")
            with open(key, "w") as f:
                f.write("synthetic-test-key\n")
            os.chmod(key, 0o600)
            env = {"PATH": os.environ.get("PATH", ""), "HOME": d, "GEN_CHUTES_ENABLED": "true",
                   "CHUTES_MODEL": "synthetic-org/synthetic-model", "CHUTES_API_KEY_FILE": key,
                   "CHUTES_LEDGER_PATH": path, "CHUTES_LEDGER_ID": lid}
            if bound is not None:
                env["HOSTED_MAX_OUTSTANDING"] = bound
            if os.environ.get("PYTHONPATH"):
                env["PYTHONPATH"] = os.environ["PYTHONPATH"]
            r = subprocess.run([sys.executable, "-c", self.STUB], cwd=HERE, env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr)
        return r.stdout.strip()

    def test_bound_is_two_under_the_brake_policy(self):
        # The Chutes sidecar always uses carried-unknown-aging-v1, which requires the retained bound two.
        self.assertEqual(self.start(None), "False 2")

    def test_retired_bound_setting_is_ignored(self):
        # HOSTED_MAX_OUTSTANDING is retired (CHUTES_MAX_CONCURRENT replaced it): any value,
        # including one the earlier code refused, is ignored with a warning.
        for value in ("2", "1", "0", "-1", "abc"):
            with self.subTest(bound=value):
                self.assertEqual(self.start(value), "False 2")


if __name__ == "__main__":
    unittest.main()
