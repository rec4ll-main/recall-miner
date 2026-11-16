"""v2p3: opt-in carried-unknown-aging-v1 (synthetic, offline). Run against the candidate source.

Aged unknowns stop holding admission slots but stay pending (the spend record is kept);
carried-unknown-v1 behaviour is unchanged.
"""
import os
import sqlite3
import sys
import tempfile
import unittest

sys.path.insert(0, os.environ.get("CANDIDATE_DIR", os.path.dirname(os.path.abspath(__file__))))
import hosted_accounting as ha  # noqa: E402

BODY = "cd" * 32


class UnknownAging(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.path = os.path.join(self.d, "ledger.sqlite")
        self.lid = ha.HostedLedger.initialize(self.path)

    def open(self, policy="carried-unknown-aging-v1", age=300):
        return ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2, recovery_policy=policy,
                                             unknown_age_out_seconds=age if policy == ha.AGING_POLICY else None)

    def unknowns(self, ledger, n, tag):
        for i in range(n):
            ledger.finish_local(ledger.begin(f"{tag}-{i}", BODY))

    def age_all(self, seconds):
        with sqlite3.connect(self.path) as conn:
            conn.execute("UPDATE hosted_attempts SET created=datetime('now', ?) WHERE state='pending'",
                         (f"-{seconds} seconds",))

    def test_three_fresh_unknowns_still_close_admission(self):
        l = self.open()
        self.unknowns(l, 3, "u")
        with self.assertRaises(ha.HostedAdmissionBlocked):
            l.begin("next", BODY)
        self.assertFalse(l.available())

    def test_aged_unknowns_free_slots_but_stay_pending(self):
        l = self.open()
        self.unknowns(l, 3, "u")
        self.age_all(301)
        self.assertTrue(l.available())
        a = l.begin("next", BODY)                 # admitted: aged rows no longer count
        l.finish_local(a)
        self.assertEqual(len(l.pending()), 4)     # nothing was cleared from the record

    def test_partially_aged_counts_only_fresh(self):
        l = self.open()
        self.unknowns(l, 2, "old")
        self.age_all(301)
        self.unknowns(l, 2, "new")               # 2 fresh unknowns: still 1 slot for an active
        a = l.begin("third", BODY)
        l.finish_local(a)                         # 3 fresh unknowns now
        with self.assertRaises(ha.HostedAdmissionBlocked):
            l.begin("fourth", BODY)

    def test_v1_policy_unchanged_by_age(self):
        l = self.open(policy="carried-unknown-v1")
        self.unknowns(l, 3, "u")
        self.age_all(100000)
        with self.assertRaises(ha.HostedAdmissionBlocked):
            l.begin("next", BODY)
        self.assertFalse(l.available())

    def test_request_key_still_never_redispatched_after_aging(self):
        l = self.open()
        self.unknowns(l, 1, "k")
        self.age_all(301)
        with self.assertRaises(ha.HostedRequestKeyConflict):
            l.begin("k-0", BODY)

    def test_age_out_validation(self):
        for bad in (None, 0, 299, True, "3600"):
            with self.assertRaises(ha.HostedAdmissionBlocked):
                ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2,
                                              recovery_policy=ha.AGING_POLICY, unknown_age_out_seconds=bad)
        with self.assertRaises(ha.HostedAdmissionBlocked):
            ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=2,
                                          recovery_policy="carried-unknown-v1", unknown_age_out_seconds=3600)
        with self.assertRaises(ha.HostedAdmissionBlocked):
            ha.HostedLedger.open_existing(self.path, self.lid, max_outstanding=1,
                                          recovery_policy=ha.AGING_POLICY, unknown_age_out_seconds=3600)


if __name__ == "__main__":
    unittest.main(verbosity=2)
