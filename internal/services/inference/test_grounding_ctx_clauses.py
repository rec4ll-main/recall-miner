"""ground_answer computes each shown context's clauses once per call.

A process-wide LRU of size 8 missed on every lookup once more than 8 contexts were
shown (claims scan contexts in order), so large requests paid the full per-claim cost
and the cache kept up to 8 large context texts alive between requests.
"""

from __future__ import annotations

import gc
import sys
import time
import types
import unittest

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import grounding  # noqa: E402

WORDS = ["harbor", "lantern", "meadow", "copper", "willow", "granite", "orchard", "falcon",
         "canyon", "ember", "glacier", "thistle", "saffron", "quarry", "beacon", "marble"]


def context(i: int, sentences: int) -> dict:
    body = " ".join(
        f"The {WORDS[(i + k) % len(WORDS)]} {WORDS[(i * 3 + k) % len(WORDS)]} keeps the "
        f"{WORDS[(i * 5 + k) % len(WORDS)]} near the {WORDS[(i * 7 + k) % len(WORDS)]}, not the "
        f"{WORDS[(i * 11 + k) % len(WORDS)]}."
        for k in range(sentences))
    return {"id": f"c{i}", "text": body}


class ContextClausesPerCallTests(unittest.TestCase):
    def test_no_process_wide_cache(self):
        self.assertFalse(hasattr(grounding._context_clauses, "cache_info"))

    def test_explicit_clauses_match_computed(self):
        ctx = context(3, 20)["text"]
        claim = "The harbor lantern keeps the meadow near the copper."
        tokens = grounding._content_tokens(claim)
        self.assertEqual(grounding.polarity_agrees(claim, tokens, ctx),
                         grounding.polarity_agrees(claim, tokens, ctx, grounding._context_clauses(ctx)))

    def test_each_context_computed_once_per_call(self):
        shown = [(i, context(i, 40)) for i in range(16)]
        answer = " ".join(f"The {w} keeps the {WORDS[(j + 3) % len(WORDS)]} near the {WORDS[(j + 5) % len(WORDS)]}."
                          for j, w in enumerate(WORDS * 2))
        calls = []
        real = grounding._context_clauses

        def counting(text):
            calls.append(text)
            return real(text)

        grounding._context_clauses = counting
        try:
            grounding.ground_answer(answer, "Which beacon keeps the marble near the quarry?", shown)
        finally:
            grounding._context_clauses = real
        self.assertEqual(len(calls), len(set(calls)), "a context's clauses were computed more than once")
        self.assertLessEqual(len(calls), len(shown))

    def test_many_large_contexts_stay_fast(self):
        shown = [(i, context(i, 600)) for i in range(16)]
        answer = " ".join(f"The {w} keeps the {WORDS[(j + 3) % len(WORDS)]} near the {WORDS[(j + 5) % len(WORDS)]}."
                          for j, w in enumerate(WORDS * 4))
        start = time.perf_counter()
        grounding.ground_answer(answer, "Which beacon keeps the marble near the quarry?", shown)
        self.assertLess(time.perf_counter() - start, 5.0)

    def test_nothing_retained_after_the_call(self):
        shown = [(i, context(i, 200)) for i in range(12)]
        grounding.ground_answer("The harbor keeps the meadow near the copper.", "What keeps the meadow?", shown)
        texts = {c["text"] for _, c in shown}
        del shown
        gc.collect()
        live = [o for o in gc.get_objects() if isinstance(o, tuple) and len(o) == 2
                and isinstance(o[0], frozenset) and isinstance(o[1], tuple) and o[1]
                and isinstance(o[1][0], tuple) and len(o[1][0]) == 2]
        self.assertEqual(live, [], "context clause tuples survived the call")
        self.assertTrue(texts)


if __name__ == "__main__":
    unittest.main()
