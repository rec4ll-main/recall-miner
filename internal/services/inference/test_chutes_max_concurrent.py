"""CHUTES_MAX_CONCURRENT: the one concurrency setting of the Chutes sidecar (synthetic, offline).

With N (default 4, valid 1..16) the Chutes sidecar runs N active generations with up to 2*N
waiting (generation limiter) and opens its ledger with N active slots under the one brake mode
(an unknown outcome stops counting after five minutes). The settings it replaced are ignored with a
setting_ignored warning. An invalid value blocks generation readiness with a plain reason; the
process still starts. The CPU inference service (Chutes disabled) keeps its GEN_MAX_* limiter.
Clean processes with synthetic disposable ledgers and keys only; nothing is dispatched.
"""
import io
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import types
import unittest

for _name, _attrs in [("sentence_transformers", ["CrossEncoder", "SentenceTransformer"]),
                      ("transformers", ["AutoModelForCausalLM", "AutoTokenizer"])]:
    if _name not in sys.modules:
        _module = types.ModuleType(_name)
        for _attr in _attrs:
            setattr(_module, _attr, type(_attr, (), {}))
        sys.modules[_name] = _module

import hosted_accounting as ha  # noqa: E402
import service  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
BODY = "ab" * 32
MODEL = "synthetic-org/synthetic-model"
INVALID = "CHUTES_MAX_CONCURRENT must be a whole number from 1 to 16"
RETIRED = ("GEN_MAX_INFLIGHT", "GEN_MAX_QUEUE", "HOSTED_MAX_OUTSTANDING", "HOSTED_MAX_CONCURRENT",
           "HOSTED_RECOVERY_POLICY", "HOSTED_UNKNOWN_AGE_OUT_SECONDS")

SCRIPT = (
    'import sys,types,json\n'
    'sys.modules.setdefault("sentence_transformers",types.SimpleNamespace(CrossEncoder=object,SentenceTransformer=object))\n'
    'sys.modules.setdefault("transformers",types.SimpleNamespace(AutoModelForCausalLM=object,AutoTokenizer=object))\n'
    'import service\n'
    'L = service.HOSTED_LEDGER\n'
    'st = service.get_generation_limiter().stats()\n'
    'out = {"enabled": service.CHUTES_CONFIG.enabled, "error": service.HOSTED_LEDGER_ERROR,\n'
    '       "limiter": [st.max_inflight, st.max_queue], "ledger": None,\n'
    '       "readiness": list(service.generation_readiness()),\n'
    '       "attempt_timeout": service.GEN_ATTEMPT_TIMEOUT}\n'
    'if L is not None:\n'
    '    out["ledger"] = {"policy": L.recovery_policy, "max_concurrent": L.max_concurrent,\n'
    '                     "age": L.unknown_age_out_seconds, "bound": L.max_outstanding}\n'
    '    if "begin" in sys.argv:\n'
    '        a = L.begin("after-upgrade", "ab" * 32); L.mark_sent(a); L.finish_local(a)\n'
    '        out["pending"] = len(L.pending())\n'
    'print(json.dumps(out))\n')


def run_service(test, extra=None, *args, chutes=True, ledger_setup=None):
    """Start service.py in a clean process; returns (output dict, stderr)."""
    with tempfile.TemporaryDirectory() as d:
        env = {"PATH": os.environ.get("PATH", ""), "HOME": d}
        if chutes:
            path = os.path.join(d, "l.sqlite")
            lid = ha.HostedLedger.initialize(path)
            if ledger_setup is not None:
                ledger_setup(path, lid)
            key = os.path.join(d, "key")
            with open(key, "w") as f:
                f.write("synthetic-test-key\n")
            os.chmod(key, 0o600)
            env.update({"GENERATION_CONNECTOR": "chutes", "GEN_CHUTES_ENABLED": "true", "CHUTES_MODEL": MODEL,
                        "CHUTES_API_KEY_FILE": key, "CHUTES_LEDGER_PATH": path, "CHUTES_LEDGER_ID": lid})
        env.update(extra or {})
        if os.environ.get("PYTHONPATH"):
            env["PYTHONPATH"] = os.environ["PYTHONPATH"]
        r = subprocess.run([sys.executable, "-c", SCRIPT, *args], cwd=HERE, env=env,
                           capture_output=True, text=True, timeout=60)
    test.assertEqual(r.returncode, 0, r.stderr)
    return json.loads(r.stdout.strip().splitlines()[-1]), r.stderr


def ignored(stderr):
    """{setting: event} for each setting_ignored line in stderr."""
    events = {}
    for line in stderr.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if isinstance(event, dict) and event.get("event") == "setting_ignored":
            events[event["setting"]] = event
    return events


class ChutesSidecarConcurrency(unittest.TestCase):
    def test_default_is_four(self):
        out, stderr = run_service(self)
        self.assertEqual(out["error"], "")
        self.assertEqual(out["readiness"], [True, ""])
        self.assertEqual(out["limiter"], [4, 8])
        self.assertEqual(out["ledger"], {"policy": ha.AGING_POLICY, "max_concurrent": 4, "age": 300, "bound": 2})
        self.assertEqual(ignored(stderr), {})

    def test_empty_value_means_default(self):
        out, _ = run_service(self, {"CHUTES_MAX_CONCURRENT": ""})
        self.assertEqual((out["limiter"], out["ledger"]["max_concurrent"]), ([4, 8], 4))

    def test_explicit_values(self):
        for value, n in (("1", 1), ("8", 8), ("16", 16), (" 2 ", 2)):
            with self.subTest(value=value):
                out, _ = run_service(self, {"CHUTES_MAX_CONCURRENT": value})
                self.assertEqual(out["error"], "")
                self.assertEqual(out["limiter"], [n, 2 * n])
                self.assertEqual(out["ledger"], {"policy": ha.AGING_POLICY, "max_concurrent": n,
                                                 "age": 300, "bound": 2})
                self.assertEqual(out["readiness"], [True, ""])

    def test_invalid_values_block_readiness_not_startup(self):
        for value in ("0", "17", "-1", "abc", "2.5", "4x", "1e1"):
            with self.subTest(value=value):
                out, stderr = run_service(self, {"CHUTES_MAX_CONCURRENT": value})
                self.assertTrue(out["enabled"])
                self.assertIsNone(out["ledger"])           # the ledger is not opened
                self.assertEqual(out["error"], INVALID)
                self.assertEqual(out["readiness"], [False, INVALID])
                self.assertIn("generation_refused", stderr)
                self.assertIn(INVALID, stderr)

    def test_invalid_value_is_reported_without_a_ledger_path(self):
        out, _ = run_service(self, {"CHUTES_MAX_CONCURRENT": "0", "CHUTES_LEDGER_PATH": ""})
        self.assertEqual(out["readiness"], [False, INVALID])

    def test_invalid_acquire_timeout_blocks_readiness_not_startup(self):
        out, _ = run_service(self, {"GEN_ACQUIRE_TIMEOUT_SECONDS": "soon"})
        reason = "GEN_ACQUIRE_TIMEOUT_SECONDS must be a number of seconds"
        self.assertEqual((out["error"], out["readiness"]), (reason, [False, reason]))

    def test_old_settings_warn_and_are_ignored(self):
        old = {"GEN_MAX_INFLIGHT": "1", "GEN_MAX_QUEUE": "0", "HOSTED_MAX_OUTSTANDING": "1",
               "HOSTED_MAX_CONCURRENT": "16", "HOSTED_RECOVERY_POLICY": "carried-unknown-aging-v1",
               "HOSTED_UNKNOWN_AGE_OUT_SECONDS": "300"}
        out, stderr = run_service(self, {**old, "CHUTES_MAX_CONCURRENT": "3"})
        self.assertEqual(out["error"], "")
        self.assertEqual(out["limiter"], [3, 6])
        self.assertEqual(out["ledger"], {"policy": ha.AGING_POLICY, "max_concurrent": 3, "age": 300, "bound": 2})
        events = ignored(stderr)
        self.assertEqual(set(events), set(RETIRED))
        for name in ("GEN_MAX_INFLIGHT", "GEN_MAX_QUEUE", "HOSTED_MAX_OUTSTANDING", "HOSTED_MAX_CONCURRENT"):
            self.assertEqual(events[name]["replacement"], "CHUTES_MAX_CONCURRENT")
        for name in RETIRED:
            self.assertIn("ignored", events[name]["detail"])

    def test_invalid_old_values_are_ignored_too(self):
        # Values the earlier code refused (blocking generation) no longer matter.
        out, stderr = run_service(self, {"HOSTED_MAX_OUTSTANDING": "abc", "HOSTED_UNKNOWN_AGE_OUT_SECONDS": "1",
                                         "HOSTED_RECOVERY_POLICY": "none", "GEN_MAX_INFLIGHT": "x"})
        self.assertEqual((out["error"], out["readiness"]), ("", [True, ""]))
        self.assertEqual(out["limiter"], [4, 8])
        self.assertEqual(len(ignored(stderr)), 4)

    def test_attempt_timeout_defaults_to_25_seconds_and_leaves_time_for_one_retry(self):
        out, _ = run_service(self)
        self.assertEqual(out["attempt_timeout"], 25.0)
        self.assertEqual((out["error"], out["readiness"]), ("", [True, ""]))
        # First attempt plus the post-dispatch retry minimum fit in the ~56 s sidecar budget
        # (58 s miner deadline minus the 2 s margin), inside the API's 60 s scoring window.
        self.assertLessEqual(service.GEN_ATTEMPT_TIMEOUT_DEFAULT + service.RETRY_POST_DISPATCH_MIN_SECONDS, 56)
        for raw, want in (("", 25.0), ("  ", 25.0), ("40", 40.0)):
            with self.subTest(raw=raw):
                out, _ = run_service(self, {"GEN_ATTEMPT_TIMEOUT_SECONDS": raw})
                self.assertEqual(out["attempt_timeout"], want)

    def test_unknown_outcome_stops_counting_after_five_minutes(self):
        out, _ = run_service(self)
        self.assertEqual(out["ledger"]["age"], 300)
        self.assertEqual(service.CHUTES_UNKNOWN_AGE_OUT_SECONDS, ha.MIN_UNKNOWN_AGE_OUT_SECONDS)

    def test_legacy_policy_setting_still_uses_the_brake_and_warns(self):
        for policy in ("legacy", "carried-unknown-v1"):
            with self.subTest(policy=policy):
                out, stderr = run_service(self, {"HOSTED_RECOVERY_POLICY": policy})
                self.assertEqual(out["ledger"], {"policy": ha.AGING_POLICY, "max_concurrent": 4,
                                                 "age": 300, "bound": 2})
                event = ignored(stderr)["HOSTED_RECOVERY_POLICY"]
                self.assertIn("five minutes", event["detail"])
                self.assertIn("no longer available", event["detail"])
                self.assertNotIn(policy, json.dumps(event))  # values are never logged

    def test_empty_old_settings_are_not_reported(self):
        _, stderr = run_service(self, {name: "" for name in RETIRED})
        self.assertEqual(ignored(stderr), {})

    def test_ledger_written_under_legacy_opens_and_admits(self):
        def legacy_history(path, lid):
            # An earlier deployment under the legacy policy: one terminal attempt and one
            # unknown outcome (no request key, no phase row), as old code wrote them.
            old = ha.HostedLedger.open_existing(path, lid, recovery_policy="legacy")
            done = old.begin()
            old.complete(done, http_status=200, usage=(1, 1))
            old.begin()
        out, _ = run_service(self, None, "begin", ledger_setup=legacy_history)
        self.assertEqual(out["error"], "")
        self.assertEqual(out["ledger"]["policy"], ha.AGING_POLICY)
        # The legacy unknown counts as one carried attempt; admission stays open.
        self.assertEqual(out["readiness"], [True, ""])
        self.assertEqual(out["pending"], 2)  # the legacy unknown is kept, plus the new one


class CpuInferenceServiceUnchanged(unittest.TestCase):
    def test_cpu_service_limiter_reads_gen_max_settings(self):
        out, stderr = run_service(self, {"GEN_MAX_INFLIGHT": "3", "GEN_MAX_QUEUE": "5",
                                         "CHUTES_MAX_CONCURRENT": "9"}, chutes=False)
        self.assertFalse(out["enabled"])
        self.assertEqual(out["limiter"], [3, 5])
        self.assertEqual(ignored(stderr), {})
        self.assertEqual(out["readiness"], [False, "generation disabled"])

    def test_cpu_service_defaults(self):
        out, _ = run_service(self, None, chutes=False)
        self.assertEqual(out["limiter"], [1, 2])

    def test_cpu_service_ignores_invalid_chutes_value(self):
        out, stderr = run_service(self, {"CHUTES_MAX_CONCURRENT": "0"}, chutes=False)
        self.assertEqual(out["limiter"], [1, 2])
        self.assertNotIn(INVALID, stderr)


class Helpers(unittest.TestCase):
    """The pure helpers, in process."""

    def test_settings(self):
        self.assertEqual(service.chutes_concurrency_settings({}), (4, 25.0, ""))
        self.assertEqual(service.chutes_concurrency_settings({"CHUTES_MAX_CONCURRENT": "16"}), (16, 25.0, ""))
        self.assertEqual(service.chutes_concurrency_settings({"GEN_ACQUIRE_TIMEOUT_SECONDS": "10"}), (4, 10.0, ""))
        for bad in ("0", "17", "999", "-3", "x", "1.0"):
            with self.subTest(bad=bad):
                self.assertEqual(service.chutes_concurrency_settings({"CHUTES_MAX_CONCURRENT": bad}),
                                 (None, None, INVALID))
        for bad in ("x", "nan", "inf"):
            with self.subTest(timeout=bad):
                self.assertEqual(service.chutes_concurrency_settings({"GEN_ACQUIRE_TIMEOUT_SECONDS": bad})[2],
                                 service.CHUTES_ACQUIRE_TIMEOUT_INVALID)

    def test_warnings_name_each_setting_once_without_values(self):
        stream = io.StringIO()
        found = service.warn_retired_chutes_settings({"GEN_MAX_QUEUE": "synthetic-value-7",
                                                      "HOSTED_RECOVERY_POLICY": "legacy",
                                                      "HOSTED_MAX_CONCURRENT": ""}, stream)
        self.assertEqual(found, ["GEN_MAX_QUEUE", "HOSTED_RECOVERY_POLICY"])
        text = stream.getvalue()
        self.assertEqual(set(ignored(text)), {"GEN_MAX_QUEUE", "HOSTED_RECOVERY_POLICY"})
        self.assertNotIn("synthetic-value-7", text)
        self.assertEqual(service.warn_retired_chutes_settings({}, io.StringIO()), [])


if __name__ == "__main__":
    unittest.main()
