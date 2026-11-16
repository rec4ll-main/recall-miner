"""v2p2: more than GEN_MAX_CONTEXTS contexts must not fail when the local reranker is absent.

Runs inside the candidate image (offline, HF_HUB_OFFLINE=1): the cross-encoder cannot load,
which is exactly the production condition that returned 500 for >4 contexts.
"""
import json
import os
import sys
import unittest

sys.path.insert(0, os.environ.get("CANDIDATE_DIR", os.path.dirname(os.path.abspath(__file__))))
os.environ.setdefault("HF_HUB_OFFLINE", "1")
os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")
import service  # noqa: E402


def ctxs(n):
    return [{"id": f"c{i}", "text": f"Item item-{i} has routing label rl-{i}."} for i in range(n)]


class ContextFallback(unittest.TestCase):
    def setUp(self):
        service._RERANK_UNAVAILABLE = False

    def test_more_than_limit_without_reranker_keeps_caller_order(self):
        import contextlib
        import io
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            shown = service._select_contexts("What routing label belongs to item item-2?", ctxs(9))
            service._select_contexts("q", ctxs(9))
        self.assertEqual([i for i, _ in shown], list(range(service.GEN_MAX_CONTEXTS)))
        self.assertTrue(service._RERANK_UNAVAILABLE)
        events = [json.loads(l) for l in out.getvalue().splitlines() if "reranker_unavailable" in l]
        self.assertEqual(len(events), 1)                 # logged once, not per request
        self.assertEqual(events[0]["fallback"], "caller_order")

    def test_unavailable_reranker_is_not_retried(self):
        service._RERANK_UNAVAILABLE = True
        called = []
        orig = service._rerank_scorer
        service._rerank_scorer = lambda q, t: called.append(1) or [0.0] * len(t)
        try:
            service._select_contexts("q", ctxs(9))
        finally:
            service._rerank_scorer = orig
        self.assertEqual(called, [])

    def test_working_scorer_still_ranks(self):
        orig = service._rerank_scorer
        service._rerank_scorer = lambda q, t: [float(i) for i in range(len(t))]  # last scores highest
        try:
            shown = service._select_contexts("q", ctxs(6))
        finally:
            service._rerank_scorer = orig
        self.assertEqual([i for i, _ in shown], [5, 4, 3, 2])

    def test_within_limit_never_reranks(self):
        shown = service._select_contexts("q", ctxs(3))
        self.assertEqual([i for i, _ in shown], [0, 1, 2])
        self.assertFalse(service._RERANK_UNAVAILABLE)


if __name__ == "__main__":
    unittest.main(verbosity=2)
