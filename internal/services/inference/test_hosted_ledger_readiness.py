"""HostedLedger.available() under contention (separate offline availability fix).

Before the fix, available() tried the ledger's one exclusive lock without waiting,
so a read overlapping another read was reported as a write in progress: a
transient false not-ready / admission refusal on a healthy, empty ledger. Now reads
share the gate, writes stay exclusive across commit and expectation publication,
each connection's continuity check (which updates _rows_seen and may latch
_broken) stays serialized, and readiness waits at most READINESS_CHECK_WAIT_SECONDS
in all, for an active or waiting write (which a waiting read never delays) or for
another connection's checks. A write that finishes within that budget is waited
out, so it no longer causes a false not-ready. These tests pause a
real connection inside the lock with events, so they do not depend on timing.
Disposable synthetic ledgers only; no provider, no live ledger.
"""

import contextlib
import os
import tempfile
import threading
import time
import unittest

from hosted_accounting import HostedAdmissionBlocked, HostedLedger


class Paused:
    """Wraps ledger.connect so the named thread blocks inside the lock until released."""

    def __init__(self, ledger, thread_name, *, write=None):
        self.ledger, self.name, self.write = ledger, thread_name, write
        self.entered, self.release = threading.Event(), threading.Event()
        self.original = ledger.connect
        self.log = []
        outer = self

        @contextlib.contextmanager
        def connect(*args, **kwargs):
            me = threading.current_thread().name
            outer.log.append(("enter", me))
            with outer.original(*args, **kwargs) as conn:
                if me == outer.name and (outer.write is None or kwargs.get("write", False) == outer.write):
                    outer.entered.set()
                    assert outer.release.wait(10), "test never released the paused thread"
                yield conn
            outer.log.append(("exit", me))

        ledger.connect = connect

    def restore(self):
        self.release.set()
        self.ledger.connect = self.original


def run(name, fn, out):
    def target():
        try:
            out.append(fn())
        except BaseException as exc:  # recorded and checked by the test
            out.append(exc)
    t = threading.Thread(target=target, name=name)
    t.start()
    return t


class ReadinessContentionTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.path = os.path.join(tmp.name, "ledger.sqlite")
        self.ledger = HostedLedger.open_existing(self.path, HostedLedger.initialize(self.path))
        self.assertTrue(self.ledger.available())

    def pause(self, name, **kw):
        p = Paused(self.ledger, name, **kw)
        self.addCleanup(p.restore)
        return p

    def test_overlapping_read_is_available_while_another_read_is_open(self):
        p = self.pause("first-reader")
        first = []
        t = run("first-reader", self.ledger.available, first)
        self.assertTrue(p.entered.wait(5))
        start = time.monotonic()
        self.assertTrue(self.ledger.available())  # the first read is still open
        self.assertLess(time.monotonic() - start, 0.5)
        p.release.set()
        t.join(5)
        self.assertEqual(first, [True])
        self.assertEqual((self.ledger.pending(), self.ledger._broken), ([], None))

    def _slow_check(self, method, thread_name):
        """Pause `thread_name` inside ledger.<method> (a serialized check); returns (inside, release, log)."""
        original = getattr(self.ledger, method)
        inside, release, log = threading.Event(), threading.Event(), []

        def check(*args):
            me = threading.current_thread().name
            log.append(("start", me))
            if me == thread_name:
                inside.set()
                assert release.wait(10)
            result = original(*args)
            log.append(("end", me))
            return result

        setattr(self.ledger, method, check)
        self.addCleanup(release.set)
        return inside, release, log

    def test_readiness_wait_behind_a_slow_check_is_bounded(self):
        # A reader stalled inside its continuity check.
        inside, release, _ = self._slow_check("_check_continuity", "slow-reader")
        first = []
        t1 = run("slow-reader", self.ledger.available, first)
        self.assertTrue(inside.wait(5))
        start = time.monotonic()
        self.assertFalse(self.ledger.available())  # not ready rather than waiting on
        waited = time.monotonic() - start
        budget = self.ledger.READINESS_CHECK_WAIT_SECONDS
        self.assertGreaterEqual(waited, budget * 0.8)
        self.assertLess(waited, budget + 0.5)
        with self.ledger._gate:
            self.assertEqual(self.ledger._readers, 1)  # the refused read released its permit
        release.set()
        t1.join(5)
        self.assertEqual(first, [True])
        self.assertTrue(self.ledger.available())  # recovers once the slow check ends
        self.assertEqual((self.ledger.pending(), self.ledger._broken), ([], None))

    def test_readiness_wait_behind_a_slow_pin_check_is_bounded(self):
        inside, release, _ = self._slow_check("_check_pin", "slow-reader")
        first = []
        t1 = run("slow-reader", self.ledger.available, first)
        self.assertTrue(inside.wait(5))
        start = time.monotonic()
        self.assertFalse(self.ledger.available())
        self.assertLess(time.monotonic() - start, self.ledger.READINESS_CHECK_WAIT_SECONDS + 0.5)
        release.set()
        t1.join(5)
        self.assertEqual(first, [True])
        self.assertTrue(self.ledger.available())

    def test_blocking_reads_still_serialize_their_checks(self):
        inside, release, log = self._slow_check("_check_continuity", "first-reader")
        first, second = [], []
        t1 = run("first-reader", self.ledger.pending, first)
        self.assertTrue(inside.wait(5))
        t2 = run("second-reader", self.ledger.pending, second)
        time.sleep(0.1)
        self.assertNotIn(("start", "second-reader"), log)  # waits for the first's check
        release.set()
        t1.join(5)
        t2.join(5)
        self.assertEqual((first, second), ([[]], [[]]))
        self.assertLess(log.index(("end", "first-reader")), log.index(("start", "second-reader")))

    def test_failed_check_releases_the_read_permit(self):
        original = self.ledger._check_identity
        calls = []

        def failing(conn):
            calls.append(1)
            if len(calls) == 1:
                raise RuntimeError("synthetic check failure")
            return original(conn)

        self.ledger._check_identity = failing
        with self.assertRaises(RuntimeError):
            self.ledger.available()
        self.ledger._check_identity = original
        with self.ledger._gate:
            self.assertEqual((self.ledger._readers, self.ledger._writing), (0, False))
        self.assertTrue(self.ledger.available())
        self.ledger.begin()  # a write is not blocked by a leaked permit

    def test_waiting_writer_bounds_the_readiness_wait(self):
        inside, release, _ = self._slow_check("_check_continuity", "slow-reader")
        first, written = [], []
        t1 = run("slow-reader", self.ledger.available, first)
        self.assertTrue(inside.wait(5))
        tw = run("writer", self.ledger.begin, written)
        for _ in range(200):
            with self.ledger._gate:
                if self.ledger._writers_waiting:
                    break
            time.sleep(0.005)
        start = time.monotonic()
        self.assertFalse(self.ledger.available())  # the queued write never got its turn in time
        self.assertLess(time.monotonic() - start, self.ledger.READINESS_CHECK_WAIT_SECONDS + 0.5)
        release.set()
        t1.join(5)
        tw.join(5)
        self.assertEqual((first, len(written)), ([True], 1))

    def _assert_stalled_write_refused_within_budget(self, start_write, in_flight=()):
        p = self.pause("writer", write=True)
        out = []
        t = run("writer", start_write, out)
        self.assertTrue(p.entered.wait(5))
        start = time.monotonic()
        # Refused because a write is still in progress after the budget, even where the
        # committed state would otherwise be available (own attempt excluded via in_flight).
        self.assertFalse(self.ledger.available(in_flight=in_flight))
        waited = time.monotonic() - start
        budget = self.ledger.READINESS_CHECK_WAIT_SECONDS
        self.assertGreaterEqual(waited, budget * 0.8, "readiness did not wait for the write")
        self.assertLess(waited, budget + 0.5, "readiness waited past its budget")
        p.release.set()
        t.join(5)
        return out

    def test_readiness_wait_on_a_stalled_begin_is_bounded(self):
        out = self._assert_stalled_write_refused_within_budget(self.ledger.begin)
        (attempt,) = out
        self.assertIsInstance(attempt, str)
        self.assertEqual(self.ledger.pending(), [attempt])
        self.assertFalse(self.ledger.available())                    # the attempt holds admission
        self.assertTrue(self.ledger.available(in_flight={attempt}))  # its own running request excluded

    def test_readiness_wait_on_a_stalled_complete_is_bounded(self):
        attempt = self.ledger.begin()
        out = self._assert_stalled_write_refused_within_budget(lambda: self.ledger.complete(attempt, usage=(1, 1)),
                                                         in_flight={attempt})
        self.assertEqual(out, [None])
        self.assertEqual(self.ledger.pending(), [])
        self.assertTrue(self.ledger.available())

    def test_readiness_waits_out_a_short_write(self):
        # The UID-228 case: a heartbeat's readiness check lands on a request's ledger write.
        # A write that ends within the budget is waited out and the committed state reported.
        attempt = self.ledger.begin()
        p = self.pause("writer", write=True)
        out = []
        t = run("writer", lambda: self.ledger.complete(attempt, usage=(1, 1)), out)
        self.assertTrue(p.entered.wait(5))
        threading.Timer(self.ledger.READINESS_CHECK_WAIT_SECONDS / 3, p.release.set).start()
        start = time.monotonic()
        self.assertTrue(self.ledger.available(in_flight={attempt}))
        self.assertLess(time.monotonic() - start, self.ledger.READINESS_CHECK_WAIT_SECONDS + 0.2)
        t.join(5)
        self.assertEqual((out, self.ledger.pending()), ([None], []))

    def test_waiting_readiness_does_not_delay_the_write(self):
        reader = self.pause("open-reader")
        read, written, waiting = [], [], []
        tr = run("open-reader", self.ledger.available, read)
        self.assertTrue(reader.entered.wait(5))
        tw = run("writer", self.ledger.begin, written)
        for _ in range(200):
            with self.ledger._gate:
                if self.ledger._writers_waiting:
                    break
            time.sleep(0.005)
        tq = run("waiting-reader", self.ledger.available, waiting)
        time.sleep(0.05)
        with self.ledger._gate:
            self.assertEqual(self.ledger._readers, 1, "a waiting readiness check took a read permit")
        reader.release.set()
        tr.join(5)
        tw.join(5)  # the write runs as soon as the open read ends, ahead of the waiting check
        tq.join(5)
        self.assertEqual((read, len(written)), ([True], 1))
        self.assertIn(waiting, ([True], [False]))

    def test_write_taking_the_lock_after_a_read_is_not_waited_on(self):
        reader = self.pause("first-reader")
        first = []
        t1 = run("first-reader", self.ledger.available, first)
        self.assertTrue(reader.entered.wait(5))
        writer = Paused(self.ledger, "writer", write=True)  # wraps the reader-pausing connect
        self.addCleanup(writer.restore)
        written, waiting = [], []
        tw = run("writer", self.ledger.begin, written)
        tr = run("waiting-reader", self.ledger.available, waiting)
        time.sleep(0.1)
        reader.release.set()
        t1.join(5)
        # Whichever got the lock next, the waiting read must finish while the write is still paused.
        tr.join(3)
        self.assertFalse(tr.is_alive(), "readiness waited past its budget for a write that took the lock")
        self.assertIn(waiting, ([True], [False]))
        if writer.entered.is_set():
            self.assertEqual(written, [])  # it really was still in its transaction
        writer.release.set()
        tw.join(5)
        self.assertEqual(first, [True])
        self.assertEqual(len(written), 1)
        self.assertFalse(self.ledger.available())

    def test_write_waits_for_open_reads(self):
        p = self.pause("open-reader")
        read, written = [], []
        tr = run("open-reader", self.ledger.available, read)
        self.assertTrue(p.entered.wait(5))
        tw = run("writer", self.ledger.begin, written)
        time.sleep(0.2)
        self.assertEqual(written, [])  # the write has not run its transaction
        self.assertFalse(self.ledger.available())  # a waiting write already refuses readiness
        p.release.set()
        tr.join(5)
        tw.join(5)
        self.assertEqual(read, [True])
        self.assertEqual(len(written), 1)
        self.assertEqual(self.ledger.pending(), written)

    def test_refused_begin_leaves_no_writer_flag(self):
        attempt = self.ledger.begin()
        with self.assertRaises(HostedAdmissionBlocked):
            self.ledger.begin()  # rolled back: an attempt is already pending
        self.assertFalse(getattr(self.ledger, "_writing", False))
        self.assertEqual(self.ledger.pending(), [attempt])
        self.assertFalse(self.ledger.available())
        self.assertTrue(self.ledger.available(in_flight={attempt}))

    def test_failed_terminal_write_stays_held(self):
        attempt = self.ledger.begin()
        original = self.ledger.connect

        @contextlib.contextmanager
        def failing(*args, **kwargs):
            with original(*args, **kwargs) as conn:
                yield conn
                if kwargs.get("write"):
                    raise OSError("synthetic commit failure")

        self.ledger.connect = failing
        try:
            with self.assertRaises(OSError):
                self.ledger.complete(attempt, usage=(1, 1))
        finally:
            self.ledger.connect = original
        self.assertFalse(getattr(self.ledger, "_writing", False))
        self.assertEqual(self.ledger.pending(), [attempt])  # nothing released or replayed
        self.assertFalse(self.ledger.available())

    def test_replaced_file_still_latches_for_concurrent_readers(self):
        os.replace(self.path, self.path + ".moved")
        with open(self.path + ".moved", "rb") as src, open(self.path, "wb") as f:
            f.write(src.read())
        results = []
        threads = [run(f"reader-{i}", self.ledger.available, results) for i in range(4)]
        for t in threads:
            t.join(5)
        self.assertEqual(results, [False] * 4)
        self.assertEqual(self.ledger._broken, "hosted ledger file replaced while running")
        self.assertFalse(self.ledger.available())


if __name__ == "__main__":
    unittest.main()
