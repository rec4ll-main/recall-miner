"""HTTP Handler + limiter integration tests.

Ported to the supported Chutes path (GEN-001 legacy test ruling): the fake
generator (_GENERATE_IMPL) is only reached after the production enabled, model,
hosted-admission and limiter checks, so each test installs isolated synthetic
admission (chutes_fixture) instead of bypassing or reordering that gate.
"""

from __future__ import annotations

import json
import sys
import threading
import time
import types
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import concurrency
import service
from chutes_fixture import SyntheticChutes
from hosted_provider import ChutesConfig


class HandlerConcurrencyTests(unittest.TestCase):
    def setUp(self):
        self.chutes = SyntheticChutes(self)
        self.lim = concurrency.GenerationLimiter(max_inflight=1, max_queue=1, acquire_timeout_seconds=0.4)
        concurrency.reset_generation_limiter_for_tests(self.lim)
        # These tests pin the limiter's own 503s; pre-dispatch retries are covered by test_retry.py.
        self._retry_backoff = service.RETRY_BACKOFF_SECONDS
        service.RETRY_BACKOFF_SECONDS = ()
        self.generate_calls = 0
        self.generate_started = threading.Event()
        self.generate_release = threading.Event()
        self.fail_once = False
        self.abort_next_handler = False

        def fake_generate(prompt, max_tokens, temperature, contexts):
            self.generate_calls += 1
            self.generate_started.set()
            if self.fail_once:
                self.fail_once = False
                raise RuntimeError("backend failure")
            if not self.generate_release.wait(timeout=5):
                raise RuntimeError("generate release timeout")
            return {
                "answer": "ok",
                "citations": [],
                "model": "fake",
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            }

        service.set_generate_impl_for_tests(fake_generate)

        test_self = self

        class TestHandler(service.Handler):
            def handle_generate(self):
                if test_self.abort_next_handler:
                    self._generation_abort = True
                return super().handle_generate()

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), TestHandler)
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        service.RETRY_BACKOFF_SECONDS = self._retry_backoff
        self.generate_release.set()
        self.httpd.shutdown()
        self.httpd.server_close()
        concurrency.reset_generation_limiter_for_tests(None)
        service.set_generate_impl_for_tests(None)


    def _wait_idle(self, timeout=2.0):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.lim.stats().active == 0 and self.lim.stats().queued == 0:
                return
            time.sleep(0.01)
        self.fail(f"limiter not idle: {self.lim.stats()}")

    def _post(self, timeout=2.0):
        body = json.dumps({"query": "q", "contexts": []}).encode()
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}/generate",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        return urllib.request.urlopen(req, timeout=timeout)

    def test_http_queued_timeout_does_not_start_abandoned_inference(self):
        self.generate_release.clear()
        t_hold = threading.Thread(target=lambda: self._post(timeout=5), daemon=True)
        t_hold.start()
        self.assertTrue(self.generate_started.wait(timeout=2))
        calls_before = self.generate_calls

        with self.assertRaises(urllib.error.HTTPError) as cm:
            self._post(timeout=2)
        self.assertEqual(cm.exception.code, 503)
        detail = cm.exception.read().decode()
        self.assertIn("timed out", detail)
        self.assertEqual(self.generate_calls, calls_before)
        st = self.lim.stats()
        self.assertEqual(st.active, 1)
        self.assertEqual(st.queued, 0)
        self.assertGreaterEqual(st.expired, 1)

        self.generate_release.set()
        t_hold.join(timeout=3)
        time.sleep(0.05)
        self.assertEqual(self.lim.stats().active, 0)

    def test_http_queued_disconnect_aborts_before_inference(self):
        self.generate_release.clear()
        t_hold = threading.Thread(target=lambda: self._post(timeout=5), daemon=True)
        t_hold.start()
        self.assertTrue(self.generate_started.wait(timeout=2))
        calls_before = self.generate_calls

        self.abort_next_handler = True
        with self.assertRaises(urllib.error.HTTPError) as cm:
            self._post(timeout=2)
        self.assertEqual(cm.exception.code, 503)
        self.assertEqual(self.generate_calls, calls_before)
        self.assertEqual(self.lim.stats().active, 1)

        self.abort_next_handler = False
        self.generate_release.set()
        t_hold.join(timeout=3)
        time.sleep(0.05)
        self.assertEqual(self.lim.stats().active, 0)

    def test_http_active_disconnect_keeps_slot_until_generate_ends(self):
        self.generate_release.clear()
        err = []

        def run():
            try:
                self._post(timeout=0.05)
            except Exception as e:
                err.append(e)

        t = threading.Thread(target=run)
        t.start()
        self.assertTrue(self.generate_started.wait(timeout=2))
        self.assertEqual(self.lim.stats().active, 1)
        time.sleep(0.1)
        self.assertEqual(self.lim.stats().active, 1)
        self.generate_release.set()
        t.join(timeout=3)
        time.sleep(0.05)
        self.assertEqual(self.lim.stats().active, 0)
        self.assertTrue(err)

    def test_http_backend_failure_releases_slot_and_recovers(self):
        # Limiter coverage only: the fake generator fails before any provider
        # dispatch. A dispatched-provider failure instead keeps a durable hold and
        # must not recover or retry automatically (see test_backend_cancel).
        self.fail_once = True
        self.generate_release.set()
        with self.assertRaises(urllib.error.HTTPError) as cm:
            self._post(timeout=2)
        self.assertEqual(cm.exception.code, 500)
        self._wait_idle()
        with self._post(timeout=2) as resp:
            self.assertEqual(resp.status, 200)
            body = json.loads(resp.read().decode())
            self.assertEqual(body["answer"], "ok")
        self._wait_idle()

    def test_http_bounds_active_and_queued(self):
        # Holders start one after the other: two hosted admission checks that
        # overlap can currently reject one of them (409; HostedLedger.available()
        # does not wait for a concurrent check). That is reproduced and proposed
        # for separate review; here the limiter bounds are what is under test.
        # A longer queue wait keeps the queued holder's 200 independent of timing.
        self.lim = concurrency.GenerationLimiter(max_inflight=1, max_queue=1, acquire_timeout_seconds=3)
        concurrency.reset_generation_limiter_for_tests(self.lim)
        self.generate_release.clear()
        holders, statuses = [], []

        def hold():
            try:
                with self._post(timeout=3) as resp:
                    statuses.append(resp.status)
            except urllib.error.HTTPError as exc:
                statuses.append(exc.code)

        for _ in range(2):
            t = threading.Thread(target=hold, daemon=True)
            holders.append(t)
            t.start()
            if len(holders) == 1:
                self.assertTrue(self.generate_started.wait(timeout=2))
            else:
                for _ in range(100):
                    if self.lim.stats().queued == 1:
                        break
                    time.sleep(0.01)
        st = self.lim.stats()
        self.assertEqual((st.active, st.queued), (1, 1))
        with self.assertRaises(urllib.error.HTTPError) as cm:
            self._post(timeout=1)
        self.assertEqual(cm.exception.code, 503)
        self.generate_release.set()
        for t in holders:
            t.join(timeout=3)
        time.sleep(0.05)
        self.assertEqual(self.lim.stats().active, 0)
        self.assertEqual(statuses, [200, 200])


    def test_disabled_connector_never_calls_generator(self):
        service.CHUTES_CONFIG = ChutesConfig()
        with self.assertRaises(urllib.error.HTTPError) as cm:
            self._post(timeout=2)
        self.assertEqual(cm.exception.code, 503)
        self.assertEqual(json.loads(cm.exception.read())["error"], "generation_disabled")
        self.assertEqual(self.generate_calls, 0)

    def test_admission_hold_never_calls_generator(self):
        self.chutes.ledger.begin()  # an unresolved attempt left by an earlier request
        with self.assertRaises(urllib.error.HTTPError) as cm:
            self._post(timeout=2)
        self.assertEqual(cm.exception.code, 409)
        self.assertEqual(json.loads(cm.exception.read())["error"], "hosted_admission_blocked")
        self.assertEqual(self.generate_calls, 0)
        st = self.lim.stats()
        self.assertEqual((st.active, st.queued), (0, 0))


class CleanImageImportSmoke(unittest.TestCase):
    def test_service_imports_concurrency_module(self):
        import importlib
        importlib.reload(concurrency)
        importlib.reload(service)
        self.assertTrue(hasattr(service, "Handler"))
        self.assertTrue(hasattr(concurrency, "GenerationLimiter"))


if __name__ == "__main__":
    unittest.main()


class RealSocketDisconnectTests(unittest.TestCase):
    """Regression: fully-sent request that disconnects while queued must not
    start a second model.generate when capacity later frees."""

    def setUp(self):
        self.chutes = SyntheticChutes(self)
        self.lim = concurrency.GenerationLimiter(max_inflight=1, max_queue=2, acquire_timeout_seconds=5.0)
        concurrency.reset_generation_limiter_for_tests(self.lim)
        self.generate_calls = 0
        self.generate_started = threading.Event()
        self.generate_release = threading.Event()
        self.call_lock = threading.Lock()

        def fake_generate(prompt, max_tokens, temperature, contexts):
            with self.call_lock:
                self.generate_calls += 1
                n = self.generate_calls
            self.generate_started.set()
            if not self.generate_release.wait(timeout=5):
                raise RuntimeError("generate release timeout")
            return {
                "answer": f"ok-{n}",
                "citations": [],
                "model": "fake",
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            }

        service.set_generate_impl_for_tests(fake_generate)
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), service.Handler)
        self.host, self.port = self.httpd.server_address
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.generate_release.set()
        self.httpd.shutdown()
        self.httpd.server_close()
        concurrency.reset_generation_limiter_for_tests(None)
        service.set_generate_impl_for_tests(None)

    def _send_raw(self, sock):
        body = b'{"query":"q","contexts":[]}'
        req = (
            b"POST /generate HTTP/1.1\r\n"
            b"Host: 127.0.0.1\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: " + str(len(body)).encode() + b"\r\n"
            b"Connection: close\r\n"
            b"\r\n" + body
        )
        sock.sendall(req)

    def test_real_socket_queued_disconnect_no_second_generate(self):
        import socket as pysocket

        # 1) Hold the active slot with a real client that stays open.
        holder = pysocket.create_connection((self.host, self.port), timeout=2)
        self._send_raw(holder)
        self.assertTrue(self.generate_started.wait(timeout=2))
        self.assertEqual(self.generate_calls, 1)

        # 2) Second client fully sends, becomes queued, then disconnects.
        queued = pysocket.create_connection((self.host, self.port), timeout=2)
        self._send_raw(queued)
        # Wait until queued (active full).
        for _ in range(50):
            if self.lim.stats().queued >= 1:
                break
            time.sleep(0.02)
        self.assertGreaterEqual(self.lim.stats().queued, 1)
        calls_before = self.generate_calls

        # Disconnect the queued client (FIN) while still waiting.
        queued.shutdown(pysocket.SHUT_RDWR)
        queued.close()
        time.sleep(0.15)  # allow select/peek to observe FIN

        # 3) Release the active holder so capacity frees.
        self.generate_release.set()
        # Drain holder response (best-effort).
        try:
            holder.settimeout(1.0)
            holder.recv(4096)
        except OSError:
            pass
        holder.close()

        # Wait until idle (active==0 AND queued==0) so active==0 is not sampled
        # during a transition. Waiter must abort, not generate.
        deadline = time.time() + 2
        while time.time() < deadline:
            st = self.lim.stats()
            if st.active == 0 and st.queued == 0:
                break
            time.sleep(0.02)
        st = self.lim.stats()
        self.assertEqual(self.generate_calls, calls_before)
        self.assertEqual(st.active, 0)
        self.assertEqual(st.queued, 0)
