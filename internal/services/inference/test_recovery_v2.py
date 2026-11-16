"""normal-recovery-v2 regressions (synthetic, offline). Run against the exact candidate source.

F1: a readiness probe that holds the active lock while it reads must not refuse a real request.
The probe's SQLite read is paused deterministically, so results never depend on timing luck.
"""
import os
import sys
import tempfile
import threading
import time
import unittest

sys.path.insert(0, os.environ.get("CANDIDATE_DIR", os.path.dirname(os.path.abspath(__file__))))
import hosted_accounting as ha  # noqa: E402

BODY = "ab" * 32  # synthetic sha256-shaped digest


class V2Recovery(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.path = os.path.join(self.d, "ledger.sqlite")
        self.lid = ha.HostedLedger.initialize(self.path)
        self.l = self.open()

    def open(self):
        return ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2,
                                             recovery_policy="carried-unknown-v1")

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

    def test_f1_probe_mid_check_does_not_refuse_begin(self):
        t, result = self.pause_probe(self.l, 0.3)
        attempt = self.l.begin("legit-request", BODY)   # v1 raised 'hosted active owner present'
        t.join(5)
        self.assertTrue(attempt)
        self.assertTrue(result["ok"])                    # the probe itself completed normally
        self.l.finish_local(attempt)
        self.assertEqual(len(self.l.pending()), 1)       # finished locally: one unknown carried

    def test_f1_slow_probe_read_longer_than_begin_wait_does_not_refuse(self):
        # The probe holds no active lock while it reads, so even a read slower than
        # ACTIVE_ACQUIRE_WAIT_SECONDS cannot refuse a real request. The write still waits for
        # the open read to finish: that is the accepted readiness-v2 rule (writes wait for open
        # reads), so the delay is the probe's own read time, never a refusal.
        t, result = self.pause_probe(self.l, 3.0)
        start = time.monotonic()
        attempt = self.l.begin("legit-request", BODY)    # v1: refused after at most 1 s of lock wait
        waited = time.monotonic() - start
        self.assertTrue(attempt)
        self.assertLess(waited, 3.5)                     # bounded by the probe's 3 s read
        t.join(10)
        self.assertTrue(result["ok"])
        self.l.finish_local(attempt)

    def test_f1_probe_never_waits_for_an_active_request(self):
        attempt = self.l.begin("active", BODY)
        start = time.monotonic()
        self.assertFalse(self.open().available())        # busy: reported, not waited for
        self.assertLess(time.monotonic() - start, 0.5)
        self.l.finish_local(attempt)

    def test_genuinely_active_request_still_refuses_after_bounded_wait(self):
        attempt = self.l.begin("active", BODY)
        other = self.open()                              # second handle, same ledger file
        start = time.monotonic()
        with self.assertRaises(ha.HostedAdmissionBlocked):
            other.begin("second", BODY)
        waited = time.monotonic() - start
        self.assertGreaterEqual(waited, 0.9)             # rode out ACTIVE_ACQUIRE_WAIT_SECONDS
        self.assertLess(waited, 3.0)                     # bounded; never hangs
        self.assertEqual(len(self.l.pending()), 1)       # nothing reserved by the refused request
        self.l.finish_local(attempt)

    def test_same_handle_second_begin_waits_then_refuses(self):
        attempt = self.l.begin("active", BODY)
        with self.assertRaises(ha.HostedAdmissionBlocked):
            self.l.begin("second", BODY)
        self.l.finish_local(attempt)

    def test_third_unknown_still_closes_admission(self):
        for k in ("u1", "u2"):
            self.l.finish_local(self.l.begin(k, BODY))   # two carried unknowns
        a = self.l.begin("fresh", BODY)                  # 2 unknown + 1 active is admitted
        self.l.finish_local(a)                           # becomes the third unknown
        with self.assertRaises(ha.HostedAdmissionBlocked):
            self.l.begin("fourth", BODY)
        self.assertFalse(self.l.available())


if __name__ == "__main__":
    unittest.main(verbosity=2)
