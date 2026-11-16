"""Runs two standalone hosted-ledger reproducers, each in its own interpreter.

The scripts are pinned by sha256 so a local edit to make them pass fails here instead.
"""

import hashlib
import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
PINNED = {
    "hosted_ledger_send_fence_cases.py": "db1aa741042a94eed04c4daa2a5a8bccdd1f1200bb0b09667422126e8d6bb135",
    "hosted_ledger_lost_ack_cases.py": "21b0efaf45b863eebe400c35e11720211e1bc72af5952df571b2cb7d5cd3d0dd",
}


class LedgerReproducers(unittest.TestCase):
    def run_script(self, name):
        with open(os.path.join(HERE, name), "rb") as f:
            self.assertEqual(hashlib.sha256(f.read()).hexdigest(), PINNED[name], "pinned reproducer changed")
        env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1",
                   PYTHONPATH=os.pathsep.join(p for p in (HERE, os.environ.get("PYTHONPATH")) if p))
        r = subprocess.run([sys.executable, os.path.join(HERE, name)], cwd=HERE, env=env,
                           capture_output=True, text=True, timeout=120)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        return r.stdout + r.stderr

    def test_send_fence_and_optional_key_regressions(self):
        out = self.run_script("hosted_ledger_send_fence_cases.py")
        self.assertIn("'requests': 0", out)

    def test_committed_but_lost_ack_regressions(self):
        self.run_script("hosted_ledger_lost_ack_cases.py")


if __name__ == "__main__":
    unittest.main()
