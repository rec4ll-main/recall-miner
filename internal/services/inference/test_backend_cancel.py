"""Cancellation/timeout propagation to the generation backend.

API cancel -> miner ctx -> model-proxy -> this service's client socket closes.
The service must then close its backend connection instead of reading a
generation nobody wants.

Ported to the supported Chutes path (GEN-001 legacy test ruling): each test runs
with synthetic enabled Chutes admission (chutes_fixture) and the backend is a
real loopback socket. What these tests observe is the local socket close and the
ledger hold. They do NOT prove that the remote provider cancelled the work or
stopped billing: a dispatched attempt stays pending (held), is never retried, and
blocks the next admission until an operator resolves it.
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
from chutes_fixture import CANONICAL_HOST, SyntheticChutes  # noqa: E402
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

    def test_abort_closes_backend_connection(self):
        service.GEN_OPENAI_TIMEOUT = 10
        t0 = time.monotonic()
        flag = {"abort": False}
        threading.Timer(0.3, lambda: flag.__setitem__("abort", True)).start()
        with self.assertRaises(service.GenerationCanceled):
            service.generate_via_openai(Q, 16, 0, should_abort=lambda: flag["abort"])
        self.assertLess(time.monotonic() - t0, 2.0)
        self.assertTrue(self.backend.disconnected.wait(2.0), "backend never saw the disconnect")
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

    def test_http_client_disconnect_propagates_to_backend(self):
        """End to end through service.Handler: drop the /generate client socket mid-inference."""
        service.GEN_OPENAI_TIMEOUT = 10
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
            self.assertTrue(self.backend.started.wait(3.0), "backend call never started")
            s.close()  # client (miner/model-proxy) gives up
            self.assertTrue(self.backend.disconnected.wait(3.0), "backend request was not cancelled")
            end = time.monotonic() + 3
            while service._HOSTED_IN_FLIGHT and time.monotonic() < end:
                time.sleep(0.02)
            self.assertEqual(service._HOSTED_IN_FLIGHT, set(), "handler never finished the attempt")
            self.assert_dispatched_once_and_held(self.chutes, self.backend)
            # The held attempt blocks the next request before any dispatch.
            s2 = socket.create_connection(("127.0.0.1", svc.server_address[1]))
            s2.sendall(b"POST /generate HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: "
                       + str(len(body)).encode() + b"\r\n\r\n" + body)
            s2.settimeout(3)
            self.assertIn(b" 409 ", s2.recv(64))
            s2.close()
            self.assertEqual(self.backend.posts, 1)
        finally:
            service._rerank_scorer = old_scorer
            svc.shutdown()
            svc.server_close()


if __name__ == "__main__":
    unittest.main()


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
    def test_abort_during_headers_closes_backend(self):
        backend = MidHeadersBackend()
        chutes = SyntheticChutes(self, backend.srv.server_address[1], timeout=10)
        try:
            self.assertTrue(backend.started.wait(0) or True)
            flag = {"abort": False}
            def arm():
                self.assertTrue(backend.started.wait(3.0))
                flag["abort"] = True
            threading.Thread(target=arm, daemon=True).start()
            t0 = time.monotonic()
            with self.assertRaises(service.GenerationCanceled):
                service.generate_via_openai(Q, 16, 0,
                                            should_abort=lambda: flag["abort"])
            self.assertLess(time.monotonic() - t0, 3.0)
            self.assertTrue(backend.disconnected.wait(2.0), "backend never saw mid-headers disconnect")
            self.assert_dispatched_once_and_held(chutes, backend)
        finally:
            backend.close()

    def test_abort_during_body_closes_backend(self):
        backend = MidBodyBackend(b'{"choices":[{"message":{"content":"partial')
        chutes = SyntheticChutes(self, backend.srv.server_address[1], timeout=10)
        try:
            flag = {"abort": False}
            def arm():
                self.assertTrue(backend.started.wait(3.0))
                time.sleep(0.2)
                flag["abort"] = True
            threading.Thread(target=arm, daemon=True).start()
            t0 = time.monotonic()
            with self.assertRaises(service.GenerationCanceled):
                service.generate_via_openai(Q, 16, 0,
                                            should_abort=lambda: flag["abort"])
            self.assertLess(time.monotonic() - t0, 3.0)
            self.assertTrue(backend.disconnected.wait(2.0), "backend never saw mid-body disconnect")
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
