"""v2p4: up to max_concurrent active hosted attempts per process (synthetic, offline).

Run against the candidate source (CANDIDATE_DIR). Each active attempt owns one slot lock;
admission needs a free slot and at most two counted pending rows that no local slot owns.
Probe pauses use the same deterministic technique as test_recovery_v2.py.
"""
import os
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from contextlib import closing, contextmanager

CANDIDATE = os.environ.get("CANDIDATE_DIR", os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, CANDIDATE)
import hosted_accounting as ha  # noqa: E402

BODY = "ef" * 32  # synthetic sha256-shaped digest
POLICY = "carried-unknown-v1"


class Concurrency(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.path = os.path.join(self.d, "ledger.sqlite")
        self.lid = ha.HostedLedger.initialize(self.path)
        self.l = self.open()
        self.addCleanup(self.release_all)

    def open(self, n=5, policy=POLICY, age=None):
        return ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2, recovery_policy=policy,
                                             unknown_age_out_seconds=age, max_concurrent=n)

    def release_all(self):
        for attempt in list(self.l._owned_attempts()):
            self.l.finish_local(attempt)

    def unknowns(self, ledger, n, tag):
        for i in range(n):
            ledger.finish_local(ledger.begin(f"{tag}-{i}", BODY))

    def phases(self):
        with closing(sqlite3.connect(self.path)) as c:
            return dict(c.execute("SELECT id, phase FROM hosted_attempt_phases"))

    def begin_all(self, ledger, keys):
        """Begin every key at once from its own thread; returns {key: attempt or exception}."""
        barrier, out = threading.Barrier(len(keys)), {}

        def run(key):
            barrier.wait()
            try:
                out[key] = ledger.begin(key, BODY)
            except Exception as exc:
                out[key] = exc
        threads = [threading.Thread(target=run, args=(k,)) for k in keys]
        for t in threads:
            t.start()
        for t in threads:
            t.join(10)
        return out

    def pause_probe(self, ledger, hold_seconds):
        """Start a readiness probe whose SQLite read is held for hold_seconds."""
        entered = threading.Event()
        real = ledger.connect

        def slow(*a, **kw):
            if threading.current_thread().name == "probe":
                entered.set()
                time.sleep(hold_seconds)
            return real(*a, **kw)

        ledger.connect = slow
        result = {}
        t = threading.Thread(target=lambda: result.setdefault("ok", ledger.available()), name="probe")
        t.start()
        self.assertTrue(entered.wait(5))
        return t, result

    def test_five_concurrent_begins_admitted_sixth_refused_after_bounded_wait(self):
        got = self.begin_all(self.l, [f"a{i}" for i in range(5)])
        self.assertTrue(all(isinstance(a, str) for a in got.values()), got)
        self.assertEqual(len(self.l._owned_attempts()), 5)
        self.assertEqual(sorted(s for s, _ in self.l._owned.values()), [0, 1, 2, 3, 4])
        self.assertFalse(self.l.available())
        self.assertTrue(self.l.busy())
        start = time.monotonic()
        with self.assertRaises(ha.HostedSlotsBusy):
            self.l.begin("sixth", BODY)
        waited = time.monotonic() - start
        self.assertGreaterEqual(waited, 0.9)              # rode out ACTIVE_ACQUIRE_WAIT_SECONDS
        self.assertLess(waited, 3.0)                      # bounded; never hangs
        self.assertEqual(len(self.l.pending()), 5)        # the refused request reserved nothing
        self.l.finish_local(got["a0"])                    # one slot frees: admitted again
        self.assertTrue(self.l.available())
        self.l.complete(self.l.begin("sixth", BODY))

    def test_two_fresh_unknowns_still_admit_up_to_n_actives(self):
        self.unknowns(self.l, 2, "u")
        got = self.begin_all(self.l, [f"a{i}" for i in range(5)])
        self.assertTrue(all(isinstance(a, str) for a in got.values()), got)
        self.assertEqual(len(self.l.pending()), 7)        # 2 carried + 5 active
        for i, attempt in enumerate(got.values()):        # actives that end terminal leave no hold
            self.l.mark_sent(attempt)
            self.l.complete(attempt, http_status=200, usage=(1, 1))
            self.l.finish_local(attempt)
        self.assertTrue(self.l.available())

    def test_three_unknowns_refuse_and_report_held_not_busy(self):
        self.unknowns(self.l, 3, "u")
        with self.assertRaises(ha.HostedAdmissionBlocked) as e:
            self.l.begin("next", BODY)
        self.assertNotIsInstance(e.exception, ha.HostedSlotsBusy)
        self.assertFalse(self.l.available())
        self.assertFalse(self.l.busy())                   # a hold, not a retryable busy refusal
        self.assertEqual(self.l._owned_attempts(), frozenset())

    def test_active_turning_unknown_closes_admission_for_later_begins(self):
        self.unknowns(self.l, 2, "u")
        a = self.l.begin("a", BODY)
        b = self.l.begin("b", BODY)                       # U is still 2: actives are owned
        self.l.mark_sent(a)
        self.l.finish_local(a)                            # a becomes the third unknown
        with self.assertRaises(ha.HostedAdmissionBlocked):
            self.l.begin("c", BODY)
        self.l.mark_sent(b)                               # the already admitted active continues
        self.l.complete(b)
        self.l.finish_local(b)
        self.assertFalse(self.l.available())

    def test_aged_unknowns_ignored(self):
        l = self.open(policy=ha.AGING_POLICY, age=300)
        self.unknowns(l, 3, "old")
        with closing(sqlite3.connect(self.path)) as c, c:
            c.execute("UPDATE hosted_attempts SET created=datetime('now','-301 seconds') WHERE state='pending'")
        self.assertTrue(l.available())
        got = self.begin_all(l, [f"a{i}" for i in range(5)])
        self.assertTrue(all(isinstance(a, str) for a in got.values()), got)
        self.assertEqual(len(l.pending()), 8)             # aged rows are kept, only uncounted
        for attempt in got.values():
            l.finish_local(attempt)

    def test_begin_does_not_mark_concurrent_actives_unknown(self):
        orphan = self.l.begin("orphan", BODY)
        self.l.mark_sent(orphan)
        self.l.finish_local(orphan)                       # carried: unknown
        sent = self.l.begin("sent", BODY)
        self.l.mark_sent(sent)
        reserved = self.l.begin("reserved", BODY)
        late = self.l.begin("late", BODY)                 # this begin reclassifies phases
        phases = self.phases()
        self.assertEqual(phases[orphan], "unknown")
        self.assertEqual(phases[sent], "dispatched")
        self.assertEqual(phases[reserved], "reserved")
        self.assertEqual(phases[late], "reserved")

    def test_unowned_pending_still_marked_unknown(self):
        # A row left behind by a crashed writer (no local slot owns it) is reclassified.
        code = ("import os,sys;sys.path.insert(0,sys.argv[3]);from hosted_accounting import HostedLedger;"
                "l=HostedLedger.open_existing(sys.argv[1],sys.argv[2],max_outstanding=2,"
                "recovery_policy='carried-unknown-v1',max_concurrent=5);a=l.begin('crashed','%s');"
                "l.mark_sent(a);os._exit(74)" % BODY)
        p = subprocess.run([sys.executable, "-c", code, self.path, self.lid, CANDIDATE])
        self.assertEqual(p.returncode, 74)
        a = self.l.begin("next", BODY)
        with closing(sqlite3.connect(self.path)) as c:
            phase = c.execute("SELECT p.phase FROM hosted_attempt_phases p JOIN hosted_attempt_requests r "
                              "USING(id) WHERE r.request_key='crashed'").fetchone()[0]
        self.assertEqual(phase, "unknown")
        self.assertEqual(self.phases()[a], "reserved")

    def test_probe_mid_read_does_not_refuse_or_delay_begin(self):
        held = [self.l.begin(f"a{i}", BODY) for i in range(4)]
        t, result = self.pause_probe(self.l, 0.3)        # took and dropped the last free slot
        attempt = self.l.begin("fifth", BODY)
        t.join(5)
        self.assertTrue(attempt)
        self.assertTrue(result["ok"])
        self.assertEqual(len(self.l._owned_attempts()), 5)
        for a in held + [attempt]:
            self.l.finish_local(a)

    def test_slow_probe_read_longer_than_begin_wait_does_not_refuse(self):
        t, result = self.pause_probe(self.l, 3.0)
        start = time.monotonic()
        attempt = self.l.begin("legit", BODY)
        self.assertTrue(attempt)
        self.assertLess(time.monotonic() - start, 3.5)    # bounded by the probe's own read
        t.join(10)
        self.assertTrue(result["ok"])
        self.l.finish_local(attempt)

    def test_probe_never_waits_when_every_slot_is_active(self):
        got = self.begin_all(self.l, [f"a{i}" for i in range(5)])
        other = self.open()
        start = time.monotonic()
        self.assertFalse(other.available())
        self.assertTrue(other.busy())
        self.assertLess(time.monotonic() - start, 0.5)
        self.l.finish_local(got["a3"])
        self.assertTrue(self.l.available())               # a slot is free; U = 1 (a3)
        self.assertFalse(other.available())               # other handle: our actives count as carried
        self.assertFalse(other.busy())

    def test_slot_released_on_complete_finish_local_and_exception_paths(self):
        l = self.open(n=2)
        a = l.begin("a", BODY)
        l.mark_sent(a)
        l.complete(a, http_status=200, usage=(1, 2))
        self.assertIn(a, l._owned_attempts())             # terminal recorded; transport not yet closed
        l.finish_local(a)
        self.assertEqual(l._owned_attempts(), frozenset())
        self.assertFalse(l.busy())
        # Refused inside the transaction (key conflict): the slot is released, nothing reserved.
        with self.assertRaises(ha.HostedRequestKeyConflict):
            l.begin("a", BODY)
        # Lost commit acknowledgement after the row landed: ownership is dropped with the slot.
        original = l._begin

        def lost_ack(*args):
            original(*args)
            raise TimeoutError("synthetic lost ack")
        l._begin = lost_ack
        with self.assertRaises(TimeoutError):
            l.begin("b", BODY)
        del l._begin
        self.assertEqual(l._owned_attempts(), frozenset())
        # finish_local write failure propagates and still releases the slot.
        c = l.begin("c", BODY)
        real = l.connect

        @contextmanager
        def broken(*a, **kw):
            raise sqlite3.OperationalError("synthetic write failure")
            yield
        l.connect = broken
        with self.assertRaises(sqlite3.OperationalError):
            l.finish_local(c)
        l.connect = real
        self.assertEqual(l._owned_attempts(), frozenset())
        # Both slots are free again: two actives admitted at once (U = 2: b and c).
        both = [l.begin("d", BODY), l.begin("e", BODY)]
        self.assertEqual(sorted(s for s, _ in l._owned.values()), [0, 1])
        for x in both:
            l.finish_local(x)

    def test_ownership_is_per_attempt(self):
        a = self.l.begin("a", BODY)
        b = self.l.begin("b", BODY)
        other = self.open()                               # another handle owns nothing
        for op in (other.mark_sent, other.complete, other.finish_local):
            with self.assertRaises(ha.HostedAdmissionBlocked):
                op(a)
        self.l.mark_sent(a)
        self.l.finish_local(a)
        for op in (self.l.mark_sent, self.l.finish_local):   # released: no longer owned
            with self.assertRaises(ha.HostedAdmissionBlocked):
                op(a)
        self.l.mark_sent(b)                               # b is unaffected
        self.l.complete(b)
        self.l.finish_local(b)
        self.assertEqual(self.phases()[a], "unknown")
        self.assertEqual(self.phases()[b], "terminal")

    def test_other_handles_actives_count_as_carried(self):
        for i in range(3):
            self.l.begin(f"a{i}", BODY)
        other = self.open()                               # cannot identify these: counted in U
        with self.assertRaises(ha.HostedAdmissionBlocked) as e:
            other.begin("x", BODY)
        self.assertNotIsInstance(e.exception, ha.HostedSlotsBusy)
        self.assertFalse(other.available())

    def test_other_process_holds_slots_shared_by_file_lock(self):
        code = ("import sys,time;sys.path.insert(0,sys.argv[3]);from hosted_accounting import HostedLedger;"
                "l=HostedLedger.open_existing(sys.argv[1],sys.argv[2],max_outstanding=2,"
                "recovery_policy='carried-unknown-v1',max_concurrent=5);"
                "a=[l.begin('child%d'%i,'" + BODY + "') for i in range(2)];print('held',flush=True);"
                "sys.stdin.readline();[l.finish_local(x) for x in a]")
        child = subprocess.Popen([sys.executable, "-c", code, self.path, self.lid, CANDIDATE],
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        try:
            self.assertEqual(child.stdout.readline().strip(), "held")
            mine = [self.l.begin(f"p{i}", BODY) for i in range(3)]   # U = 2 (the child's rows)
            with self.assertRaises(ha.HostedSlotsBusy):
                self.l.begin("p3", BODY)                  # 2 + 3 slots in use across processes
            for a in mine:
                self.l.finish_local(a)
        finally:
            child.communicate("go\n", timeout=10)
        self.assertEqual(child.returncode, 0)

    def test_symlinked_extra_slot_refused(self):
        os.symlink(self.path, self.path + ".active-lock.1")
        a = self.l.begin("a", BODY)                       # slot 0 is valid and free
        with self.assertRaises(ha.HostedAdmissionBlocked) as e:
            self.l.begin("b", BODY)
        self.assertNotIsInstance(e.exception, ha.HostedSlotsBusy)
        self.assertFalse(self.l.available())
        self.assertEqual(len(self.l.pending()), 1)
        self.l.finish_local(a)

    def test_default_is_one_slot_exactly(self):
        l = ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2, recovery_policy=POLICY)
        self.assertEqual(l.max_concurrent, 1)
        a = l.begin("a", BODY)
        self.assertEqual((l._active_attempt, l._active_fd is not None), (a, True))
        with self.assertRaises(ha.HostedSlotsBusy):
            l.begin("b", BODY)
        l.finish_local(a)
        self.assertEqual((l._active_attempt, l._active_fd), (None, None))
        self.assertFalse([f for f in os.listdir(self.d) if f.startswith("ledger.sqlite.active-lock.")])

    def test_bound_validation(self):
        for bad in (0, 17, True, "5", 2.0, None):
            with self.assertRaises(ha.HostedAdmissionBlocked):
                self.open(n=bad)
        with self.assertRaises(ha.HostedAdmissionBlocked):
            ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2, max_concurrent=2)
        self.assertEqual(self.open(n=16).max_concurrent, 16)


if __name__ == "__main__":
    unittest.main(verbosity=2)
