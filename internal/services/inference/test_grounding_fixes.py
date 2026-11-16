"""Paraphrase / unsupported numbers / negation.

Grounding must:
  * accept paraphrased but answerable questions (construction year ↔ built in 1873);
  * abstain when the answer invents a number the contexts do not carry;
  * abstain when the answer negates a fact the contexts affirm.
"""

from __future__ import annotations

import json
from pathlib import Path
import sys
import types
import unittest

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import grounding  # noqa: E402
import service  # noqa: E402

LIGHT = {"id": "light", "text": "The Zephyrine Lighthouse on Kelvar Island was built in 1873 by engineer Matilda Orsk."}
WATER = {"id": "water", "text": "Water freezes at zero degrees Celsius at standard pressure."}
STOUT = {"id": "stout", "text": "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels."}


class ParaphraseCoverageTests(unittest.TestCase):
    def test_paraphrased_construction_year_is_answerable(self):
        shown = [(0, LIGHT)]
        out = service.ground_answer(
            "The Zephyrine Lighthouse was constructed in 1873 [1].",
            "In which year was the Zephyrine Lighthouse constructed?",
            shown)
        self.assertFalse(out["abstained"], out)
        self.assertEqual([c["context_id"] for c in out["citations"]], ["light"])

    def test_paraphrased_builder_question_is_answerable(self):
        shown = [(0, LIGHT)]
        out = service.ground_answer(
            "Matilda Orsk engineered the Zephyrine Lighthouse [1].",
            "Which engineer constructed the Zephyrine Lighthouse?",
            shown)
        self.assertFalse(out["abstained"], out)


    def test_two_paraphrased_verbs_are_answerable(self):
        # erected/constructed both paraphrase "built": literal coverage was 4/6.
        out = service.ground_answer(
            "Matilda Orsk erected the Zephyrine Lighthouse on Kelvar Island in 1873 [1].",
            "Who erected the Zephyrine Lighthouse on Kelvar Island, and when was it constructed?",
            [(0, LIGHT)])
        self.assertFalse(out["abstained"], out)

    def test_paraphrased_departure_time_is_answerable(self):
        ferry = {"id": "ferry", "text": "The Kelvar Island ferry departs from Port Ashby twice daily, at 07:15 and 16:40."}
        out = service.ground_answer(
            "The Kelvar Island ferry leaves Port Ashby at 07:15 and 16:40 [1].",
            "At what time does the Kelvar Island ferry leave Port Ashby?",
            [(0, ferry)])
        self.assertFalse(out["abstained"], out)

    def test_paraphrased_unanswerable_price_still_abstains(self):
        out = service.ground_answer(
            "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels [1].",
            "What does a bottle of the Bluefin flagship stout retail for?",
            [(0, STOUT)])
        self.assertTrue(out["abstained"], out)

    def test_unknown_proper_noun_anchor_abstains(self):
        # Varnholt is not in any context: 3/4 literal coverage passed before.
        out = service.ground_answer(
            "The lighthouse on Varnholt Island was built in 1873 by Matilda Orsk [1].",
            "Who built the lighthouse on Varnholt Island?",
            [(0, LIGHT)])
        self.assertTrue(out["abstained"], out)
        self.assertEqual(out["abstain_reason"], "question_not_covered")


class UnsupportedNumberTests(unittest.TestCase):
    def test_invented_price_number_abstains(self):
        shown = [(0, STOUT)]
        out = service.ground_answer(
            "Bluefin Brewery's flagship stout costs $14 [1].",
            "How much does Bluefin Brewery's flagship stout cost?",
            shown)
        self.assertTrue(out["abstained"], out)

    def test_invented_year_in_otherwise_supported_answer_abstains(self):
        shown = [(0, LIGHT)]
        out = service.ground_answer(
            "The Zephyrine Lighthouse was built in 1901 by Matilda Orsk [1].",
            "Who built the Zephyrine Lighthouse and in what year?",
            shown)
        self.assertTrue(out["abstained"], out)

    def test_supported_number_still_passes(self):
        shown = [(0, STOUT)]
        out = service.ground_answer(
            "Bluefin Brewery's flagship stout is aged for 19 months [1].",
            "How long is Bluefin Brewery's flagship stout aged?",
            shown)
        self.assertFalse(out["abstained"], out)


class NegationTests(unittest.TestCase):
    def test_negated_freezing_point_abstains(self):
        shown = [(0, WATER)]
        out = service.ground_answer(
            "Water does not freeze at zero degrees Celsius [1].",
            "At what temperature does water freeze?",
            shown)
        self.assertTrue(out["abstained"], out)

    def test_yes_contradicting_negative_context_abstains(self):
        ctx = {"id": "sun", "text": "The Kelvar Island ferry does not run on Sundays."}
        out = service.ground_answer(
            "Yes, the Kelvar Island ferry runs on Sundays [1].",
            "Does the Kelvar Island ferry run on Sundays?",
            [(0, ctx)])
        self.assertTrue(out["abstained"], out)

    def test_leading_no_with_matching_polarity_is_kept(self):
        ctx = {"id": "sun", "text": "The Kelvar Island ferry does not run on Sundays."}
        out = service.ground_answer(
            "No, the Kelvar Island ferry does not run on Sundays [1].",
            "Does the Kelvar Island ferry run on Sundays?",
            [(0, ctx)])
        self.assertFalse(out["abstained"], out)

    def test_negated_builder_claim_abstains(self):
        shown = [(0, LIGHT)]
        out = service.ground_answer(
            "The Zephyrine Lighthouse was not built by Matilda Orsk [1].",
            "Who built the Zephyrine Lighthouse?",
            shown)
        self.assertTrue(out["abstained"], out)

    def test_context_with_negation_still_answerable(self):
        ctx = {"id": "n", "text": "The Halvard bridge was not designed by Matilda Orsk; it was designed by Ilse Brandt."}
        out = service.ground_answer(
            "The Halvard bridge was not designed by Matilda Orsk [1].",
            "Was the Halvard bridge designed by Matilda Orsk?",
            [(0, ctx)])
        self.assertFalse(out["abstained"], out)



class RequestedPriceTests(unittest.TestCase):
    def test_requested_price_fixtures(self):
        fixtures = json.loads((Path(__file__).parent / "testdata" / "requested_price_fixtures.json").read_text())
        for case in fixtures:
            with self.subTest(case=case["name"]):
                out = service.ground_answer(case["answer"] + " [1]", case["query"], list(enumerate(case["contexts"])))
                self.assertEqual(out["abstained"], case["expect_abstained"], out)
                if out["abstained"]:
                    self.assertEqual(out["citations"], [])

    def test_missing_price_forces_zero_coverage(self):
        coverage, uncovered, _ = service.question_coverage(
            "How much does Bluefin Brewery's flagship stout cost?", [(0, STOUT)])
        self.assertEqual(coverage, 0.0)
        self.assertIn("cost", uncovered)


CAPITAL = {"id": "cap", "text": "The capital of Veldoria is Corvast, a port city on the Amber Coast."}
BRIDGES = {"id": "br", "text": "The Halvard bridge, among the oldest bridges in Corvast, spans the Amber river."}
FERRY = {"id": "ferry", "text": "The Kelvar Island ferry crossing takes 40 minutes."}


class ContrastClauseTests(unittest.TestCase):
    def test_trailing_not_clause_is_not_a_negated_claim(self):
        out = service.ground_answer(
            "The capital of Veldoria is Corvast, not Brimholt [1].",
            "What is the capital of Veldoria?",
            [(0, CAPITAL)])
        self.assertFalse(out["abstained"], out)
        self.assertEqual([c["context_id"] for c in out["citations"]], ["cap"])

    def test_rather_than_number_is_not_an_asserted_number(self):
        out = service.ground_answer(
            "The Kelvar Island ferry crossing takes 40 minutes rather than 90 minutes [1].",
            "How long does the Kelvar Island ferry crossing take?",
            [(0, FERRY)])
        self.assertFalse(out["abstained"], out)

    def test_instead_of_number_is_not_an_asserted_number(self):
        out = service.ground_answer(
            "The Kelvar Island ferry crossing takes 40 minutes instead of 1,200 [1].",
            "How long does the Kelvar Island ferry crossing take?",
            [(0, FERRY)])
        self.assertFalse(out["abstained"], out)

    def test_inline_negation_still_abstains(self):
        out = service.ground_answer(
            "The capital of Veldoria is not Corvast [1].",
            "What is the capital of Veldoria?",
            [(0, CAPITAL)])
        self.assertTrue(out["abstained"], out)

    def test_invented_number_before_contrast_still_abstains(self):
        out = service.ground_answer(
            "The Kelvar Island ferry crossing takes 55 minutes, not 40 [1].",
            "How long does the Kelvar Island ferry crossing take?",
            [(0, FERRY)])
        self.assertTrue(out["abstained"], out)
        self.assertEqual(out["abstain_reason"], "unsupported_number")

    def test_asserted_text_strips_only_a_trailing_contrast(self):
        self.assertEqual(grounding.asserted_text("It is Corvast, not Brimholt."), "It is Corvast")
        self.assertEqual(grounding.asserted_text("It is 5 km rather than 7 km"), "It is 5 km")
        self.assertEqual(grounding.asserted_text("It is not Brimholt."), "It is not Brimholt.")
        self.assertEqual(grounding.asserted_text("Not Brimholt."), "Not Brimholt.")


class IdiomaticOneTests(unittest.TestCase):
    def test_one_of_the_is_not_the_number_one(self):
        out = service.ground_answer(
            "The Halvard bridge is one of the oldest bridges in Corvast [1].",
            "Is the Halvard bridge old?",
            [(0, BRIDGES)])
        self.assertFalse(out["abstained"], out)

    def test_idioms_drop_one_but_quantities_keep_it(self):
        for text in ("one of the oldest bridges", "one such bridge", "no one crossed the bridge",
                     "the towns trade with one another"):
            with self.subTest(text=text):
                self.assertNotIn("1", grounding.number_tokens(text))
        for text in ("one bridge spans the river", "1 member joined", "One bridge spans the river"):
            with self.subTest(text=text):
                self.assertIn("1", grounding.number_tokens(text))

    def test_genuine_one_quantity_still_needs_support(self):
        out = service.ground_answer(
            "One bridge, the Halvard bridge, spans the Amber river in Corvast [1].",
            "Which bridge spans the Amber river?",
            [(0, BRIDGES)])
        self.assertTrue(out["abstained"], out)
        self.assertEqual(out["abstain_reason"], "unsupported_number")


if __name__ == "__main__":
    unittest.main()
