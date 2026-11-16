"""Subject linking: a context about another named entity must not support the subject.

Two shown contexts can tie on question coverage, one naming the subject and one
holding the asked attribute for a different entity. Grounding must not let the
second lend its value to the subject (cross-chunk confabulation), while chunks
that name the subject among others, or name no entity, still count.
"""

from __future__ import annotations

import sys
import types
import unittest

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import service  # noqa: E402

PUDAM = {"id": "pudam", "text": "The Pudam orchestra has 509 members."}
SKEVUK = {"id": "skevuk", "text": "The Skevuk league was founded in the year 1810."}
SKEVUK_BARE = {"id": "skevuk-bare", "text": "Skevuk league was founded in the year 1810."}
VARNEK = {"id": "varnek", "text": "The Varnek bridge is a stone bridge over the river."}
PRONOUN = {"id": "pronoun", "text": "It was completed in the year 1874."}
PARTICIPLE = {"id": "participle", "text": "Completed in the year 1874, it spans the river."}
SHARED = {"id": "shared", "text": "The Varnek bridge, designed by Talen Morvik, was completed in the year 1874."}


class SubjectLinkingTests(unittest.TestCase):
    def test_other_entity_attribute_is_not_attached_to_the_subject(self):
        out = service.ground_answer(
            "The Pudam orchestra was founded in the year 1810 [2].",
            "In what year was the Pudam orchestra founded?",
            [(0, PUDAM), (1, SKEVUK)])
        self.assertTrue(out["abstained"], out)

    def test_other_entity_attribute_without_markers_is_not_attached(self):
        out = service.ground_answer(
            "The Pudam orchestra was founded in the year 1810.",
            "In what year was the Pudam orchestra founded?",
            [(0, SKEVUK), (1, PUDAM)])
        self.assertTrue(out["abstained"], out)

    def test_pronoun_chunk_still_supports_the_subject(self):
        out = service.ground_answer(
            "The Varnek bridge was completed in the year 1874 [2].",
            "In what year was the Varnek bridge completed?",
            [(0, VARNEK), (1, PRONOUN)])
        self.assertFalse(out["abstained"], out)
        self.assertEqual([c["context_id"] for c in out["citations"]], ["pronoun"])

    def test_chunk_naming_the_subject_among_others_still_supports(self):
        out = service.ground_answer(
            "The Varnek bridge was completed in the year 1874 [1].",
            "In what year was the Varnek bridge completed?",
            [(0, SHARED), (1, SKEVUK)])
        self.assertFalse(out["abstained"], out)
        self.assertEqual([c["context_id"] for c in out["citations"]], ["shared"])

    def test_sentence_initial_entity_is_not_attached_to_the_subject(self):
        # The name starts the sentence (no determiner), so it must still count as a name.
        for shown in ([(0, PUDAM), (1, SKEVUK_BARE)], [(0, SKEVUK_BARE), (1, PUDAM)]):
            for answer in ("The Pudam orchestra was founded in the year 1810 [2].",
                           "The Pudam orchestra was founded in the year 1810."):
                out = service.ground_answer(answer, "In what year was the Pudam orchestra founded?", shown)
                self.assertTrue(out["abstained"], (answer, shown, out))

    def test_participle_chunk_still_supports_the_subject(self):
        out = service.ground_answer(
            "The Varnek bridge was completed in the year 1874 [2].",
            "In what year was the Varnek bridge completed?",
            [(0, VARNEK), (1, PARTICIPLE)])
        self.assertFalse(out["abstained"], out)
        self.assertEqual([c["context_id"] for c in out["citations"]], ["participle"])

    def test_proper_noun_tokens_keep_sentence_initial_names_but_not_function_words(self):
        for text in ("It was completed in 1874. The span is long.",
                     "Completed in 1874, it spans the river.",
                     "There are many bridges. Although old, it stands.",
                     "In 1874 the span opened. Today it carries traffic."):
            self.assertEqual(service.proper_noun_tokens(text), set(), text)
        skevuk = service._content_tokens("Skevuk").pop()
        self.assertIn(skevuk, service.proper_noun_tokens(SKEVUK["text"]))
        self.assertIn(skevuk, service.proper_noun_tokens(SKEVUK_BARE["text"]))
        self.assertIn(skevuk, service.proper_noun_tokens("Founded in 1810, Skevuk league plays today."))

if __name__ == "__main__":
    unittest.main()
