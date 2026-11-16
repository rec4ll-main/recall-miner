"""Unit tests for GenerationLimiter."""

from __future__ import annotations

import threading
import time
import unittest

from concurrency import GenerationLimiter, GenerationOverloaded


class GenerationLimiterTests(unittest.TestCase):
    def test_rejects_when_inflight_and_queue_full(self):
        lim = GenerationLimiter(max_inflight=1, max_queue=1, acquire_timeout_seconds=0.2)
        lim.acquire()
        entered = threading.Event()

        def waiter():
            try:
                lim.acquire()
                entered.set()
                time.sleep(0.05)
            finally:
                try:
                    lim.release()
                except RuntimeError:
                    pass

        t = threading.Thread(target=waiter)
        t.start()
        time.sleep(0.02)
        with self.assertRaises(GenerationOverloaded):
            lim.acquire()
        lim.release()
        t.join(timeout=2)
        self.assertTrue(entered.wait(timeout=1))

    def test_acquire_timeout_rejects_without_freeing_active(self):
        lim = GenerationLimiter(max_inflight=1, max_queue=2, acquire_timeout_seconds=0.05)
        lim.acquire()
        with self.assertRaises(GenerationOverloaded):
            lim.acquire()
        st = lim.stats()
        self.assertEqual(st.active, 1)
        self.assertEqual(st.queued, 0)
        self.assertGreaterEqual(st.expired, 1)
        lim.release()
        self.assertEqual(lim.stats().active, 0)

    def test_queue_abort_before_inference_never_takes_slot(self):
        lim = GenerationLimiter(max_inflight=1, max_queue=2, acquire_timeout_seconds=2)
        lim.acquire()
        abort = threading.Event()

        def waiter():
            with self.assertRaises(GenerationOverloaded):
                lim.acquire(should_abort=abort.is_set)

        t = threading.Thread(target=waiter)
        t.start()
        time.sleep(0.05)
        abort.set()
        t.join(timeout=2)
        self.assertEqual(lim.stats().active, 1)
        lim.release()

    def test_concurrent_race_respects_max_active(self):
        lim = GenerationLimiter(max_inflight=2, max_queue=2, acquire_timeout_seconds=1)
        active_peak = 0
        lock = threading.Lock()
        barrier = threading.Barrier(8)

        def worker():
            nonlocal active_peak
            barrier.wait()
            try:
                lim.acquire()
            except GenerationOverloaded:
                return
            try:
                with lock:
                    # stats.active includes this holder
                    active_peak = max(active_peak, lim.stats().active)
                time.sleep(0.02)
            finally:
                lim.release()

        threads = [threading.Thread(target=worker) for _ in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertLessEqual(active_peak, 2)
        self.assertEqual(lim.stats().active, 0)

    def test_release_only_after_work_slot_held_during_failure(self):
        lim = GenerationLimiter(max_inflight=1, max_queue=0, acquire_timeout_seconds=0.1)
        lim.acquire()
        self.assertEqual(lim.stats().active, 1)
        try:
            raise RuntimeError("backend boom")
        except RuntimeError:
            pass
        finally:
            lim.release()
        self.assertEqual(lim.stats().active, 0)

    def test_restart_semantics_new_limiter_resets_counters(self):
        a = GenerationLimiter(max_inflight=1, max_queue=0, acquire_timeout_seconds=0.1)
        a.acquire()
        b = GenerationLimiter(max_inflight=1, max_queue=0, acquire_timeout_seconds=0.1)
        b.acquire()
        self.assertEqual(b.stats().active, 1)
        a.release()
        b.release()

    def test_default_env_timeout_is_finite(self):
        import os
        os.environ.pop("GEN_ACQUIRE_TIMEOUT_SECONDS", None)
        lim = GenerationLimiter.from_env()
        self.assertGreater(lim._acquire_timeout, 0)


if __name__ == "__main__":
    unittest.main()


class GenerationLimiterDeadlineWakeupTests(unittest.TestCase):
    def test_wakeup_after_deadline_does_not_admit(self):
        # Deterministic post-wakeup path: synchronize into Condition.wait,
        # hold the condition lock across expiry, free the slot under that lock, then
        # allow resumption. This cannot pass via ordinary timeout inside the
        # capacity-full loop — the waiter only reacquires after capacity is free.
        lim = GenerationLimiter(max_inflight=1, max_queue=2, acquire_timeout_seconds=0.2)
        lim.acquire()
        admitted_before = lim.stats().admitted
        completed_before = lim.stats().completed
        result = {"err": None}

        def waiter():
            try:
                lim.acquire()
                lim.release()
            except GenerationOverloaded as exc:
                result["err"] = exc

        t = threading.Thread(target=waiter)
        t.start()
        for _ in range(100):
            if lim.stats().queued == 1:
                break
            time.sleep(0.005)
        self.assertEqual(lim.stats().queued, 1)

        # Waiter is inside Condition.wait (lock released). Hold the lock across
        # deadline expiry so it cannot finish the capacity-full loop check.
        with lim._cond:
            deadline = time.monotonic() + lim._acquire_timeout + 0.05
            while time.monotonic() < deadline:
                time.sleep(0.01)
            # Free slot under the held lock (lim.release would deadlock).
            self.assertGreaterEqual(lim._active, 1)
            lim._active -= 1
            lim._completed += 1
            lim._cond.notify()
        # Waiter resumes with capacity free but past deadline → before-admission timeout.
        t.join(timeout=2)
        self.assertIsNotNone(result["err"])
        err = str(result["err"])
        self.assertIn("timed out", err)
        self.assertIn("before admission", err)
        st = lim.stats()
        self.assertEqual(st.admitted, admitted_before)
        self.assertEqual(st.active, 0)
        self.assertEqual(st.queued, 0)
        self.assertEqual(st.completed, completed_before + 1)
        self.assertGreaterEqual(st.expired, 1)
