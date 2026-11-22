"""The ML stack is imported on first local embed/rerank only, never at module load.

The Chutes sidecar runs service.py but never embeds or reranks locally; importing
torch/sentence-transformers/transformers at load cost it about 400 MB per miner.
"""
import os
import subprocess
import sys
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))

PROBE = textwrap.dedent('''
    import importlib.abc, sys
    BLOCKED = ("torch", "sentence_transformers", "transformers")
    class Block(importlib.abc.MetaPathFinder):
        def find_spec(self, name, path=None, target=None):
            if name.split(".")[0] in BLOCKED:
                raise ImportError("blocked in test: " + name)
            return None
    sys.meta_path.insert(0, Block())
    import service
    loaded = [m for m in BLOCKED if m in sys.modules]
    assert not loaded, loaded
    try:
        service.get_reranker()
    except ImportError as e:
        assert "sentence_transformers" in str(e), e
    else:
        raise AssertionError("get_reranker() did not import the ML stack")
    print("ok")
''')


class MLStackLazyTest(unittest.TestCase):
    def test_service_imports_without_ml_stack(self):
        env = dict(os.environ, GENERATION_CONNECTOR="chutes")
        r = subprocess.run([sys.executable, "-c", PROBE], cwd=HERE, env=env,
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr[-2000:])
        self.assertEqual(r.stdout.strip().splitlines()[-1], "ok")


if __name__ == "__main__":
    unittest.main()
