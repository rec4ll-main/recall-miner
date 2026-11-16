"""Question coverage + per-citation support verification.

Regression for the E2E unanswerable failure: "How much does a bottle of
Bluefin Brewery's flagship stout cost?" was answered with the (true, cited)
ageing fact of the stout chunk and not flagged. Plus unsupported-citation
fixtures: a citation whose ID exists but whose chunk does not support the
claim must be dropped, and an answer left without support must abstain.
"""

from __future__ import annotations

import sys
import types
import unittest

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import service  # noqa: E402

E2E = [
    ("lighthouse", "The Zephyrine Lighthouse on Kelvar Island was built in 1873 by engineer Matilda Orsk."),
    ("ferry", "The Kelvar Island ferry departs from Port Ashby twice daily, at 07:15 and 16:40."),
    ("stout", "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels."),
    ("project", "Project Orange-Mango-7 rotates its access code every 45 minutes."),
    ("garden", "The Quillon botanical garden houses 412 species of alpine moss."),
    ("bridge", "The Halvard suspension bridge has a main span of 1,288 metres."),
    ("choir", "The Ostrava Lantern Choir was founded by conductor Ilse Brandt in 1952."),
    ("comet", "Comet Varlund-3 returns to the inner solar system every 61 years."),
]
CTX = [{"id": k, "text": t} for k, t in E2E]
ALL = list(enumerate(CTX))
VAL = [
    {"id": "ctx-1", "text": "Paris is the capital of France and its largest city. The Seine river runs through it."},
    {"id": "ctx-2", "text": "The Earth orbits the Sun once every 365.25 days. This period is called a year."},
    {"id": "ctx-3", "text": "Water freezes at zero degrees Celsius at standard pressure."},
]


def shown_of(*keys):
    return [(i, c) for i, c in ALL if c["id"] in keys]


def ids(out):
    return [c["context_id"] for c in out["citations"]]


class UnanswerableRegressionTests(unittest.TestCase):
    """The exact E2E unanswerable set; every restatement must abstain."""

    STOUT_COST = "How much does a bottle of Bluefin Brewery's flagship stout cost?"

    def test_stout_cost_restatement_abstains(self):
        # The literal failing output from evidence/gen-eval/patched-v4.json.
        for shown in (ALL, shown_of("stout", "lighthouse", "ferry", "choir")):
            for ans in (
                "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels.",
                "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels [1].",
            ):
                out = service.ground_answer(ans, self.STOUT_COST, shown)
                self.assertTrue(out["abstained"], (ans, out))
                self.assertEqual(out["citations"], [])
                self.assertEqual(out["abstain_reason"], "question_not_covered")
                self.assertIn("cost", out["uncovered"])

    def test_ferry_engine_restatement_abstains(self):
        out = service.ground_answer(
            "The Kelvar Island ferry departs from Port Ashby at 07:15 and 16:40 [2].",
            "Which company manufactures the engines of the Kelvar Island ferry?", ALL)
        self.assertTrue(out["abstained"], out)

    def test_mars_population_any_answer_abstains(self):
        for ans in ("The Quillon botanical garden houses 412 species of alpine moss [5].",
                    "About 412 people live there."):
            out = service.ground_answer(ans, "What is the population of the Mars colony Zorblax-9?", ALL)
            self.assertTrue(out["abstained"], out)

    def test_bridge_designer_confabulation_abstains(self):
        out = service.ground_answer(
            "The Halvard suspension bridge was designed by engineer Matilda Orsk [1][6].",
            "Who designed the Halvard suspension bridge?", ALL)
        self.assertTrue(out["abstained"], out)

    def test_stout_price_with_real_price_context_is_answerable(self):
        ctx = CTX + [{"id": "price", "text": "A bottle of Bluefin Brewery's flagship stout costs $14."}]
        out = service.ground_answer("A bottle of the flagship stout costs $14 [9].", self.STOUT_COST, list(enumerate(ctx)))
        self.assertFalse(out["abstained"], out)
        self.assertEqual(ids(out), ["price"])


class AnswerableStillAnswered(unittest.TestCase):
    CASES = [
        ("Who built the Zephyrine Lighthouse, and in what year?",
         "The Zephyrine Lighthouse was built in 1873 by engineer Matilda Orsk [1].", "lighthouse"),
        ("How long is Bluefin Brewery's flagship stout aged, and in what kind of barrels?",
         "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels [3].", "stout"),
        ("Who founded the Ostrava Lantern Choir?", "Ilse Brandt founded the Ostrava Lantern Choir in 1952 [7].", "choir"),
        ("When does the Kelvar Island ferry depart from Port Ashby?",
         "The Kelvar Island ferry departs from Port Ashby at 07:15 and 16:40 [2].", "ferry"),
        ("How many species of alpine moss does the Quillon botanical garden house?",
         "The Quillon botanical garden houses 412 species of alpine moss [5].", "garden"),
        ("How often does Comet Varlund-3 return to the inner solar system?",
         "Comet Varlund-3 returns to the inner solar system every 61 years [8].", "comet"),
    ]

    def test_e2e_answerable_set(self):
        for q, a, key in self.CASES:
            out = service.ground_answer(a, q, ALL)
            self.assertFalse(out["abstained"], (q, out))
            self.assertEqual(ids(out), [key], q)

    def test_validator_corpus_shapes(self):
        cases = [
            ("What is the capital of France?", "The capital of France is Paris [1].", [VAL[0]], ["ctx-1"]),
            ("How long does it take the Earth to orbit the Sun?", "The Earth orbits the Sun every 365.25 days [1].", [VAL[1]], ["ctx-2"]),
            ("At what temperature does water freeze?", "Water freezes at zero degrees Celsius [1].", [VAL[2]], ["ctx-3"]),
        ]
        for q, a, ctx, want in cases:
            out = service.ground_answer(a, q, list(enumerate(ctx)))
            self.assertFalse(out["abstained"], (q, out))
            self.assertEqual(ids(out), want)

    def test_validator_abstention_shape(self):
        out = service.ground_answer("Water freezes at 0 degrees Celsius.", "At what temperature does water freeze?", [(0, VAL[0])])
        self.assertTrue(out["abstained"])

    def test_generic_summary_skips_coverage(self):
        cov, unc, generic = service.question_coverage("Summarize the three provided facts.", list(enumerate(VAL)))
        self.assertTrue(generic)
        self.assertEqual(cov, 1.0)


class UnsupportedCitationFixtures(unittest.TestCase):
    """Citation IDs that exist but whose chunk does not support the claim."""

    def test_wrong_marker_is_dropped_and_replaced_by_supporting_chunk(self):
        out = service.ground_answer(
            "The Zephyrine Lighthouse was built in 1873 by Matilda Orsk [8].",
            "Who built the Zephyrine Lighthouse?", ALL)
        self.assertFalse(out["abstained"], out)
        self.assertEqual(ids(out), ["lighthouse"])
        self.assertEqual(out["dropped_citations"], ["comet"])

    def test_wrong_marker_without_supporting_chunk_abstains(self):
        out = service.ground_answer(
            "The Zephyrine Lighthouse was built in 1873 by Matilda Orsk [2].",
            "Who built the Zephyrine Lighthouse?", shown_of("lighthouse", "ferry", "comet")[1:])
        self.assertTrue(out["abstained"], out)
        self.assertEqual(out["citations"], [])

    def test_extra_irrelevant_marker_is_dropped(self):
        out = service.ground_answer(
            "Comet Varlund-3 returns every 61 years [8][3][5].",
            "How often does Comet Varlund-3 return?", ALL)
        self.assertFalse(out["abstained"], out)
        self.assertEqual(ids(out), ["comet"])
        self.assertEqual(sorted(out["dropped_citations"]), ["garden", "stout"])

    def test_hallucinated_second_claim_with_real_citation_abstains(self):
        out = service.ground_answer(
            "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels [3]. "
            "It costs 9 euros [3].",
            "How long is Bluefin Brewery's flagship stout aged?", ALL)
        self.assertTrue(out["abstained"], out)
        self.assertEqual(out["abstain_reason"], "unsupported_claim")
        self.assertEqual(out["citations"], [])

    def test_hallucinated_claim_caught_even_when_whole_answer_support_passes(self):
        # Whole-answer support is 4/7 >= 0.5; only the per-claim check fails it.
        q = "How long is Bluefin Brewery's flagship stout aged?"
        a = "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels [3]. It costs 9 euros [3]."
        out = service.ground_answer(a, q, ALL)
        self.assertTrue(out["abstained"])
        self.assertEqual(out["dropped_citations"], ["stout"])

    def test_two_claims_two_chunks_each_cited_correctly(self):
        out = service.ground_answer(
            "The ferry departs at 07:15 and 16:40 [2]. The stout is aged 19 months in cedar barrels [3].",
            "When does the Kelvar ferry depart and how long is the Bluefin stout aged?", ALL)
        self.assertFalse(out["abstained"], out)
        self.assertEqual(ids(out), ["ferry", "stout"])

    def test_swapped_markers_are_corrected_or_dropped(self):
        out = service.ground_answer(
            "The ferry departs at 07:15 and 16:40 [3]. The stout is aged 19 months in cedar barrels [2].",
            "When does the Kelvar ferry depart and how long is the Bluefin stout aged?", ALL)
        self.assertFalse(out["abstained"], out)
        self.assertEqual(ids(out), ["ferry", "stout"])
        self.assertEqual(out["dropped_citations"], ["stout", "ferry"])
        # every returned citation must support its claim
        for cid in ids(out):
            text = {c["id"]: c["text"] for c in CTX}[cid]
            self.assertTrue(service._content_tokens(out["answer"]) & service._content_tokens(text))

    def test_citation_supports_thresholds(self):
        claim = service._content_tokens("built in 1873 by Matilda Orsk")
        self.assertTrue(service.citation_supports(claim, claim, service._content_tokens(E2E[0][1])))
        self.assertFalse(service.citation_supports(claim, claim, service._content_tokens(E2E[7][1])))
        # one shared generic token is not enough
        self.assertFalse(service.citation_supports(
            service._content_tokens("Kelvar Island has a famous zoo"),
            service._content_tokens("famous zoo"),
            service._content_tokens(E2E[1][1])))

    def test_dropped_citations_reach_http_payload_keys(self):
        self.assertIn("dropped_citations", service.GROUNDING_KEYS)
        self.assertIn("uncovered", service.GROUNDING_KEYS)


if __name__ == "__main__":
    unittest.main()
