"""Credential-free rendering of the synthetic Chutes Compose overlay.

testdata/compose.chutes-synthetic.yml is a test fixture, not deployment topology.
It is rendered over the repository docker-compose.yml with `docker compose config
--format json` only: never up, pull or build, no daemon, no Docker socket, no
user config. The subprocess gets an explicit minimal environment, an empty
temporary HOME and DOCKER_CONFIG, and --env-file /dev/null. Run it where an
already cached Docker CLI with the Compose plugin exists (network none, non-root);
without one it skips locally and fails under CI, and nothing is downloaded.

Rendering only proves configuration shape. Runtime refusal and admission are
covered by the paired service tests (LegacyConnectorRefusal in
test_compose_generation.py, chutes_fixture-based handler tests).
"""
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
BASE = ROOT / "docker-compose.yml"
OVERLAY = HERE / "testdata" / "compose.chutes-synthetic.yml"
MODEL = "synthetic-org/synthetic-model"
LEDGER_ID = "00000000-0000-4000-8000-000000000001"


def synthetic_env(home, **overrides):
    env = {
        "PATH": os.environ.get("PATH", ""),
        "HOME": home,
        "DOCKER_CONFIG": os.path.join(home, ".docker"),
        "MINER_INTERNAL_API_KEY": "synthetic-render-only",
        "SYNTHETIC_CHUTES_MODEL": MODEL,
        "SYNTHETIC_CHUTES_LEDGER_ID": LEDGER_ID,
        "SYNTHETIC_CHUTES_SECRET_DIR": "/synthetic/secret-dir",
        "SYNTHETIC_CHUTES_STATE_DIR": "/synthetic/state-dir",
    }
    for key, value in overrides.items():
        if value is None:
            env.pop(key, None)
        else:
            env[key] = value
    return env


def compose_argv(docker="docker"):
    # The base file defines both Chutes services under the opt-in `chutes` profile.
    return [docker, "compose", "--env-file", "/dev/null", "-f", str(BASE), "-f", str(OVERLAY),
            "--profile", "chutes", "config", "--format", "json"]


def render(docker="docker", **overrides):
    with tempfile.TemporaryDirectory() as home:
        os.makedirs(os.path.join(home, ".docker"))
        return subprocess.run(compose_argv(docker), cwd=str(ROOT), env=synthetic_env(home, **overrides),
                              capture_output=True, text=True, timeout=30)


def compose_available():
    if not shutil.which("docker"):
        return False
    try:
        with tempfile.TemporaryDirectory() as home:
            r = subprocess.run(["docker", "compose", "version"], env={"PATH": os.environ.get("PATH", ""), "HOME": home,
                               "DOCKER_CONFIG": home}, capture_output=True, timeout=10)
        return r.returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


class ComposeChutesSyntheticRender(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not compose_available():
            if os.environ.get("CI"):
                raise AssertionError("cached docker compose plugin required in CI; nothing is downloaded")
            raise unittest.SkipTest("cached docker compose plugin unavailable")

    def services(self, **overrides):
        r = render(**overrides)
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout)["services"]

    def test_default_render_is_disabled(self):
        gen = self.services()["chutes-generation"]["environment"]
        self.assertEqual(gen["GEN_CHUTES_ENABLED"], "false")
        self.assertEqual(gen["GENERATION_CONNECTOR"], "chutes")

    def test_missing_required_variables_fail_before_launch(self):
        for name in ("SYNTHETIC_CHUTES_MODEL", "SYNTHETIC_CHUTES_LEDGER_ID", "SYNTHETIC_CHUTES_SECRET_DIR",
                     "SYNTHETIC_CHUTES_STATE_DIR", "MINER_INTERNAL_API_KEY"):
            with self.subTest(missing=name):
                r = render(**{name: None})
                self.assertNotEqual(r.returncode, 0)
                self.assertIn(name, r.stderr)

    def test_legacy_url_cannot_activate_a_generic_connector(self):
        services = self.services(GEN_OPENAI_URL="http://synthetic-backend.invalid:8000/v1", SYNTHETIC_CHUTES_ENABLED="true")
        self.assertEqual(services["miner-inference-service"]["environment"]["GEN_OPENAI_URL"], "")
        self.assertNotIn("GEN_OPENAI_URL", services["chutes-generation"]["environment"])
        self.assertNotIn("generate", services["cpu-model-proxy"]["environment"]["CAPABILITIES"].split(","))

    def test_enabled_model_capability_and_routing(self):
        s = self.services(SYNTHETIC_CHUTES_ENABLED="true")
        gen, proxy, cpu = (s[n]["environment"] for n in ("chutes-generation", "chutes-genproxy", "cpu-model-proxy"))
        self.assertEqual(gen["GEN_CHUTES_ENABLED"], "true")
        self.assertEqual((gen["CHUTES_MODEL"], proxy["MODELS"]), (MODEL, MODEL))
        self.assertEqual(proxy["CAPABILITIES"], "generate")
        self.assertEqual(proxy["UPSTREAM_URL"], "http://chutes-generation:19201")
        self.assertEqual(proxy["UPSTREAM_HEALTH_PATH"], "/health/generation")
        self.assertEqual(gen["LISTEN_ADDR"], ":19201")
        # Embedding/rerank stay on the existing CPU path, without the generation model.
        self.assertEqual(cpu["CAPABILITIES"], "embedding,rerank")
        self.assertNotIn(MODEL, cpu["MODELS"])
        self.assertEqual(cpu["UPSTREAM_URL"], "http://miner-inference-service:9200")
        self.assertEqual(s["model-proxy"]["environment"]["CAPABILITIES"], "embedding")

    def test_credential_and_state_mounts(self):
        gen = self.services()["chutes-generation"]
        mounts = {v["target"]: v for v in gen["volumes"]}
        cred, state = mounts["/run/synthetic-chutes"], mounts["/state"]
        self.assertEqual(gen["environment"]["CHUTES_API_KEY_FILE"], "/run/synthetic-chutes/api-key")
        # Newer Compose omits create_host_path when it is false.
        create = lambda m: m.get("bind", {}).get("create_host_path", False)
        self.assertEqual((cred["type"], cred.get("read_only"), create(cred)), ("bind", True, False))
        self.assertEqual((state["type"], bool(state.get("read_only")), create(state)), ("bind", False, False))
        self.assertNotEqual(cred["source"], state["source"])
        self.assertTrue(gen["environment"]["CHUTES_LEDGER_PATH"].startswith("/state/"))
        self.assertEqual(gen["environment"]["CHUTES_LEDGER_ID"], LEDGER_ID)
        # The ledger never lives on the model cache or any other service's volume.
        others = {v.get("source") if isinstance(v, dict) else str(v).split(":", 1)[0]
                  for n, svc in self.services().items() if n != "chutes-generation" for v in svc.get("volumes", [])}
        self.assertNotIn(state["source"], others)

    def test_hardening_retained(self):
        gen = self.services()["chutes-generation"]
        self.assertEqual(gen["user"], "1001:1001")
        self.assertTrue(gen["read_only"])
        self.assertEqual(gen["cap_drop"], ["ALL"])
        self.assertIn("no-new-privileges:true", gen["security_opt"])
        self.assertTrue(any(str(t).startswith("/tmp") for t in gen["tmpfs"]))

    def test_one_concurrency_setting(self):
        gen = self.services()["chutes-generation"]["environment"]
        self.assertEqual(gen["CHUTES_MAX_CONCURRENT"], "4")
        self.assertEqual(self.services(CHUTES_MAX_CONCURRENT="12")["chutes-generation"]["environment"]
                         ["CHUTES_MAX_CONCURRENT"], "12")
        # The settings it replaced are no longer forwarded, even when the environment holds them.
        old = {"HOSTED_RECOVERY_POLICY": "legacy", "HOSTED_UNKNOWN_AGE_OUT_SECONDS": "300",
               "HOSTED_MAX_OUTSTANDING": "1", "HOSTED_MAX_CONCURRENT": "2", "GEN_MAX_INFLIGHT": "2",
               "GEN_MAX_QUEUE": "2"}
        gen = self.services(**old)["chutes-generation"]["environment"]
        for name in old:
            self.assertNotIn(name, gen)

    def test_deadline_wiring(self):
        s = self.services()
        self.assertEqual(s["chutes-generation"]["environment"]["GEN_OPENAI_TIMEOUT_SECONDS"], "58")
        miner = s["miner"]["environment"]
        self.assertEqual(miner["GENERATION_BACKEND_TIMEOUT_SECONDS"], "58")
        self.assertEqual(miner["GENERATION_SIDECAR_DEADLINE_MARGIN_MS"], "2000")

    def test_no_key_material_rendered(self):
        s = self.services(SYNTHETIC_CHUTES_ENABLED="true")
        for name, svc in s.items():
            env = svc.get("environment") or {}
            self.assertNotIn("CHUTES_API_KEY", env, name)
            for key, value in env.items():
                if key == "INTERNAL_API_KEY":
                    self.assertEqual(value, "synthetic-render-only")
                elif re.search(r"(^|_)(API_KEY|SECRET|TOKEN|PASSWORD)$", key):
                    # Base pass-throughs (e.g. vllm HF_TOKEN) stay empty: none is supplied.
                    self.assertEqual(value, "", f"{name}: credential-shaped variable {key} has a value")


class OverlayStartupPairing(unittest.TestCase):
    """Runtime side of the rendered shape (clean processes; no Compose or Docker needed)."""

    STUB = ('import sys,types\n'
            'sys.modules.setdefault("sentence_transformers",types.SimpleNamespace(CrossEncoder=object,SentenceTransformer=object))\n'
            'sys.modules.setdefault("transformers",types.SimpleNamespace(AutoModelForCausalLM=object,AutoTokenizer=object))\n'
            'import service\n'
            'print(service.CHUTES_CONFIG.enabled, service.HOSTED_LEDGER is None, service.HOSTED_LEDGER_ERROR != "")\n')

    def start(self, **values):
        with tempfile.TemporaryDirectory() as home:
            env = {"PATH": os.environ.get("PATH", ""), "HOME": home, "GENERATION_CONNECTOR": "chutes",
                   "CHUTES_MODEL": MODEL, "GEN_OPENAI_TIMEOUT_SECONDS": "85", **values}
            if os.environ.get("PYTHONPATH"):
                env["PYTHONPATH"] = os.environ["PYTHONPATH"]
            return subprocess.run([sys.executable, "-c", self.STUB], cwd=str(HERE), env=env,
                                  capture_output=True, text=True, timeout=60)

    def test_default_disabled_starts_without_generation(self):
        r = self.start(GEN_CHUTES_ENABLED="false")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.split(), ["False", "True", "True"])

    def test_enabled_without_credential_file_refuses_to_start(self):
        r = self.start(GEN_CHUTES_ENABLED="true", CHUTES_API_KEY_FILE="/run/synthetic-chutes/absent",
                       CHUTES_LEDGER_PATH="/state/chutes-ledger.sqlite", CHUTES_LEDGER_ID=LEDGER_ID)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("Chutes credential file unavailable", r.stderr)

    def test_enabled_with_missing_ledger_blocks_generation_only(self):
        with tempfile.TemporaryDirectory() as d:
            key = os.path.join(d, "api-key")
            with open(key, "w") as f:
                f.write("synthetic-test-key\n")
            os.chmod(key, 0o600)
            r = self.start(GEN_CHUTES_ENABLED="true", CHUTES_API_KEY_FILE=key,
                           CHUTES_LEDGER_PATH=os.path.join(d, "missing.sqlite"), CHUTES_LEDGER_ID=LEDGER_ID)
        self.assertEqual(r.returncode, 0, r.stderr)  # embedding/rerank keep serving
        self.assertEqual(r.stdout.split(), ["True", "True", "True"])  # generation blocked: no ledger


class ComposeHarness(unittest.TestCase):
    """A fake docker checks only this harness (argv and environment); it does not render Compose."""

    def test_harness_runs_config_only_with_a_minimal_environment(self):
        with tempfile.TemporaryDirectory() as d:
            record = os.path.join(d, "record.json")
            fake = os.path.join(d, "docker")
            with open(fake, "w") as f:
                f.write("#!%s\nimport json, os, sys\njson.dump({'argv': sys.argv[1:], 'env': dict(os.environ)}, "
                        "open(%r, 'w'))\nprint(json.dumps({'services': {}}))\n" % (sys.executable, record))
            os.chmod(fake, stat.S_IRWXU)
            r = render(docker=fake)
            self.assertEqual(r.returncode, 0, r.stderr)
            with open(record) as f:
                seen = json.load(f)
        argv = seen["argv"]
        self.assertEqual(argv[:3], ["compose", "--env-file", "/dev/null"])
        self.assertEqual(argv[-3:], ["config", "--format", "json"])
        self.assertFalse({"up", "pull", "build", "run", "start"} & set(argv))
        self.assertEqual(set(seen["env"]) - {"PWD", "SHLVL", "_", "OLDPWD", "__CF_USER_TEXT_ENCODING", "LC_CTYPE"},
                         {"PATH", "HOME", "DOCKER_CONFIG", "MINER_INTERNAL_API_KEY", "SYNTHETIC_CHUTES_MODEL",
                          "SYNTHETIC_CHUTES_LEDGER_ID", "SYNTHETIC_CHUTES_SECRET_DIR", "SYNTHETIC_CHUTES_STATE_DIR"})
        self.assertTrue(seen["env"]["DOCKER_CONFIG"].startswith(seen["env"]["HOME"]))
        self.assertNotEqual(seen["env"]["HOME"], os.path.expanduser("~"))

    def test_overlay_is_marked_synthetic_and_holds_no_secrets(self):
        text = OVERLAY.read_text()
        self.assertTrue(text.startswith("# SYNTHETIC TEST FIXTURE - NOT A DEPLOYMENT FILE."))
        for forbidden in ("CHUTES_API_KEY:", "cpk_", "Bearer ", "/root/", "/home/"):
            self.assertNotIn(forbidden, text)


if __name__ == "__main__":
    unittest.main()
