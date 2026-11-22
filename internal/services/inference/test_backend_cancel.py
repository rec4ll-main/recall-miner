"""Caller disconnect, timeout and failure behaviour of a dispatched generation attempt.

Before the request is sent, a caller disconnect (API -> miner ctx -> model-proxy ->
this service's client socket) stops the request and nothing is sent. Once the
request is sent it no longer does: the attempt is read to its end or to the
request deadline, so its outcome is recorded instead of left unresolved (an unresolved
attempt counts toward the hold limit and makes the sidecar report not ready).

Ported to the supported Chutes path (GEN-001 legacy test ruling): each test runs
with synthetic enabled Chutes admission (chutes_fixture) and the backend is a
real loopback socket. A timed-out or failed dispatched attempt stays pending (held),
is never retried, and blocks the next admission until it ages out or is resolved.
"""

from __future__ import annotations

import json
import select
import socket
import sys
import threading
import time
import types
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import service  # noqa: E402
from chutes_fixture import CANONICAL_HOST, SyntheticChutes, completion  # noqa: E402
from hosted_provider import ChutesConfig, HostedProviderError  # noqa: E402

Q = [{"role": "user", "content": "q"}]


class HoldAssertions:
    """Shared checks for an attempt that reached the backend and then was abandoned."""

    def assert_dispatched_once_and_held(self, chutes, backend):
        self.assertEqual(chutes.requested, [(CANONICAL_HOST, None)])
        self.assertEqual(len(chutes.connections), 1, "transport retried")
        self.assertEqual(backend.posts, 1, "backend saw a retry")
        self.assertEqual(len(chutes.pending()), 1, "dispatched attempt must stay held")
        self.assertFalse(chutes.admission_open(), "next admission must be blocked by the hold")


    def assert_dispatched_once_and_resolved(self, chutes, backend):
        self.assertEqual(chutes.requested, [(CANONICAL_HOST, None)])
        self.assertEqual(len(chutes.connections), 1, "transport retried")
        self.assertEqual(backend.posts, 1, "backend saw a retry")
        self.assertEqual(chutes.pending(), [], "finished attempt must not stay held")
        self.assertTrue(chutes.admission_open(), "a finished attempt must not block admission")


class AnswerAfterBackend:
    """Fake provider that answers a valid completion after ``delay`` seconds; records an
    early client disconnect (which the sidecar must no longer cause once dispatched)."""

    def __init__(self, delay: float, content: str = "The lighthouse was built in 1873 by Matilda Orsk."):
        self.disconnected = threading.Event()
        self.started = threading.Event()
        self.answered = threading.Event()
        self.posts = 0
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                outer.posts += 1
                outer.started.set()
                sock = self.connection
                end = time.monotonic() + delay
                while time.monotonic() < end:
                    r, _, _ = select.select([sock], [], [], 0.02)
                    if r:
                        try:
                            if sock.recv(1, socket.MSG_PEEK) == b"":
                                outer.disconnected.set()
                                return
                        except OSError:
                            outer.disconnected.set()
                            return
                body = json.dumps(completion(content)).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                outer.answered.set()

            def log_message(self, *a):
                return

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class SlowBackend:
    """Fake vLLM that never answers; records when its client disconnects."""

    def __init__(self):
        self.disconnected = threading.Event()
        self.started = threading.Event()
        self.posts = 0
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                outer.posts += 1
                outer.started.set()
                sock = self.connection
                end = time.monotonic() + 10
                while time.monotonic() < end:
                    r, _, _ = select.select([sock], [], [], 0.05)
                    if r:
                        try:
                            if sock.recv(1, socket.MSG_PEEK) == b"":
                                outer.disconnected.set()
                                return
                        except OSError:
                            outer.disconnected.set()
                            return

            def log_message(self, *a):
                return

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}/v1"

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class MalformedBackend:
    """Reads the request, then answers with correctly framed but malformed JSON."""

    def __init__(self):
        self.posts = 0
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                outer.posts += 1
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", "9")
                self.end_headers()
                self.wfile.write(b'{"choices')

            def log_message(self, *a):
                return

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class BackendCancelTests(HoldAssertions, unittest.TestCase):
    def setUp(self):
        self.backend = SlowBackend()
        self.addCleanup(self.backend.close)
        self.chutes = SyntheticChutes(self, self.backend.srv.server_address[1])

    def test_abort_after_dispatch_finishes_and_records(self):
        backend = AnswerAfterBackend(0.8)
        self.addCleanup(backend.close)
        chutes = SyntheticChutes(self, backend.srv.server_address[1])
        flag = {"abort": False}
        def arm():
            self.assertTrue(backend.started.wait(3.0))
            flag["abort"] = True
        threading.Thread(target=arm, daemon=True).start()
        answer, _, _ = service.generate_via_openai(Q, 16, 0, should_abort=lambda: flag["abort"])
        self.assertTrue(flag["abort"], "abort was never raised during the attempt")
        self.assertIn("1873", answer)
        self.assertFalse(backend.disconnected.is_set(), "a dispatched attempt was abandoned")
        self.assert_dispatched_once_and_resolved(chutes, backend)

    def test_abort_after_dispatch_is_still_bounded_by_the_deadline(self):
        service.GEN_OPENAI_TIMEOUT = 0.8
        t0 = time.monotonic()
        with self.assertRaises(TimeoutError):
            service.generate_via_openai(Q, 16, 0, should_abort=lambda: self.backend.started.is_set())
        self.assertGreater(time.monotonic() - t0, 0.6, "the abort, not the deadline, ended the attempt")
        self.assertLess(time.monotonic() - t0, 2.5)
        self.assertTrue(self.backend.disconnected.wait(2.0))
        self.assert_dispatched_once_and_held(self.chutes, self.backend)

    def test_backend_timeout_is_bounded_and_closes(self):
        service.GEN_OPENAI_TIMEOUT = 0.5
        t0 = time.monotonic()
        with self.assertRaises(TimeoutError):
            service.generate_via_openai(Q, 16, 0)
        self.assertLess(time.monotonic() - t0, 2.0)
        self.assertTrue(self.backend.disconnected.wait(2.0))
        self.assert_dispatched_once_and_held(self.chutes, self.backend)

    def test_backend_failure_after_dispatch_is_not_retried(self):
        backend = MalformedBackend()
        self.addCleanup(backend.close)
        chutes = SyntheticChutes(self, backend.srv.server_address[1])
        with self.assertRaises(HostedProviderError):
            service.generate_via_openai(Q, 16, 0)
        self.assert_dispatched_once_and_held(chutes, backend)

    def test_abort_before_dispatch_sends_nothing(self):
        with self.assertRaises(service.GenerationCanceled):
            service.generate_via_openai(Q, 16, 0, should_abort=lambda: True)
        time.sleep(0.2)
        self.assertEqual(self.backend.posts, 0, "pre-dispatch abort created remote work")
        self.assertEqual(self.chutes.pending(), [])
        self.assertTrue(self.chutes.admission_open())

    def test_disabled_connector_makes_no_connection(self):
        service.CHUTES_CONFIG = ChutesConfig()
        with self.assertRaises(HostedProviderError):
            service.generate_via_openai(Q, 16, 0)
        self.assertEqual((self.chutes.connections, self.backend.posts), ([], 0))

    def test_http_client_disconnect_does_not_abandon_dispatched_attempt(self):
        """End to end through service.Handler: drop the /generate client socket mid-inference."""
        backend = AnswerAfterBackend(0.8)
        self.addCleanup(backend.close)
        chutes = SyntheticChutes(self, backend.srv.server_address[1])
        svc = ThreadingHTTPServer(("127.0.0.1", 0), service.Handler)
        threading.Thread(target=svc.serve_forever, daemon=True).start()
        service.set_generate_impl_for_tests(None)
        old_scorer = service._rerank_scorer
        service._rerank_scorer = lambda q, texts: [0.0] * len(texts)
        try:
            body = json.dumps({"query": "Who built the lighthouse?", "contexts": [
                {"id": "a", "text": "The lighthouse was built in 1873 by Matilda Orsk."}]}).encode()
            s = socket.create_connection(("127.0.0.1", svc.server_address[1]))
            s.sendall(b"POST /generate HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: "
                      + str(len(body)).encode() + b"\r\n\r\n" + body)
            self.assertTrue(backend.started.wait(3.0), "backend call never started")
            s.close()  # client (miner/model-proxy) gives up
            self.assertTrue(backend.answered.wait(3.0), "dispatched attempt was abandoned")
            self.assertFalse(backend.disconnected.is_set())
            end = time.monotonic() + 3
            while service._HOSTED_IN_FLIGHT and time.monotonic() < end:
                time.sleep(0.02)
            self.assertEqual(service._HOSTED_IN_FLIGHT, set(), "handler never finished the attempt")
            self.assert_dispatched_once_and_resolved(chutes, backend)
        finally:
            service._rerank_scorer = old_scorer
            svc.shutdown()
            svc.server_close()


class MidHeadersBackend:
    """Fake vLLM that sends status line then stalls before finishing headers."""

    def __init__(self):
        self.disconnected = threading.Event()
        self.started = threading.Event()
        self.posts = 0
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                outer.posts += 1
                outer.started.set()
                # Partial response: status line only. getresponse() blocks waiting
                # for the rest of the headers.
                try:
                    self.connection.sendall(b"HTTP/1.1 200 OK\r\n")
                except OSError:
                    outer.disconnected.set()
                    return
                sock = self.connection
                end = time.monotonic() + 10
                while time.monotonic() < end:
                    r, _, _ = select.select([sock], [], [], 0.05)
                    if r:
                        try:
                            if sock.recv(1, socket.MSG_PEEK) == b"":
                                outer.disconnected.set()
                                return
                        except OSError:
                            outer.disconnected.set()
                            return

            def log_message(self, *a):
                return

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}/v1"

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class MidBodyBackend:
    """Fake vLLM that finishes headers then hangs mid-body."""

    def __init__(self, body_prefix: bytes):
        self.disconnected = threading.Event()
        self.started = threading.Event()
        self.posts = 0
        self.body_prefix = body_prefix
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                outer.posts += 1
                outer.started.set()
                try:
                    # Claim a large Content-Length then send only a prefix.
                    self.connection.sendall(
                        b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
                        b"Content-Length: 100000\r\n\r\n" + outer.body_prefix
                    )
                except OSError:
                    outer.disconnected.set()
                    return
                sock = self.connection
                end = time.monotonic() + 10
                while time.monotonic() < end:
                    r, _, _ = select.select([sock], [], [], 0.05)
                    if r:
                        try:
                            if sock.recv(1, socket.MSG_PEEK) == b"":
                                outer.disconnected.set()
                                return
                        except OSError:
                            outer.disconnected.set()
                            return

            def log_message(self, *a):
                return

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}/v1"

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class MidResponseCancelTests(HoldAssertions, unittest.TestCase):
    def test_abort_during_headers_waits_for_the_deadline(self):
        backend = MidHeadersBackend()
        chutes = SyntheticChutes(self, backend.srv.server_address[1], timeout=0.8)
        try:
            self.assertTrue(backend.started.wait(0) or True)
            flag = {"abort": False}
            def arm():
                self.assertTrue(backend.started.wait(3.0))
                flag["abort"] = True
            threading.Thread(target=arm, daemon=True).start()
            t0 = time.monotonic()
            with self.assertRaises(TimeoutError):
                service.generate_via_openai(Q, 16, 0,
                                            should_abort=lambda: flag["abort"])
            self.assertGreater(time.monotonic() - t0, 0.6, "the abort, not the deadline, ended the attempt")
            self.assertLess(time.monotonic() - t0, 3.0)
            self.assertTrue(backend.disconnected.wait(2.0), "backend never saw the deadline close")
            self.assert_dispatched_once_and_held(chutes, backend)
        finally:
            backend.close()

    def test_abort_during_body_waits_for_the_deadline(self):
        backend = MidBodyBackend(b'{"choices":[{"message":{"content":"partial')
        chutes = SyntheticChutes(self, backend.srv.server_address[1], timeout=0.8)
        try:
            flag = {"abort": False}
            def arm():
                self.assertTrue(backend.started.wait(3.0))
                time.sleep(0.2)
                flag["abort"] = True
            threading.Thread(target=arm, daemon=True).start()
            t0 = time.monotonic()
            with self.assertRaises(TimeoutError):
                service.generate_via_openai(Q, 16, 0,
                                            should_abort=lambda: flag["abort"])
            self.assertGreater(time.monotonic() - t0, 0.6, "the abort, not the deadline, ended the attempt")
            self.assertLess(time.monotonic() - t0, 3.0)
            self.assertTrue(backend.disconnected.wait(2.0), "backend never saw the deadline close")
            self.assert_dispatched_once_and_held(chutes, backend)
        finally:
            backend.close()

    def test_timeout_during_body_closes_backend(self):
        backend = MidBodyBackend(b'{"choices":[{"message":{"content":"partial')
        chutes = SyntheticChutes(self, backend.srv.server_address[1], timeout=0.8)
        try:
            t0 = time.monotonic()
            with self.assertRaises(TimeoutError):
                service.generate_via_openai(Q, 16, 0)
            self.assertLess(time.monotonic() - t0, 2.5)
            self.assertTrue(backend.disconnected.wait(2.0), "backend never saw timeout disconnect")
            self.assert_dispatched_once_and_held(chutes, backend)
        finally:
            backend.close()
