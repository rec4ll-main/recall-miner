"""Grounded citation + abstention post-processing."""

from __future__ import annotations

import json
import sys
import threading
import types
import unittest
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import service  # noqa: E402
from chutes_fixture import CANONICAL_HOST, SYNTHETIC_MODEL, SyntheticChutes, completion  # noqa: E402

CTX = [
    {"id": "c-light", "text": "The Zephyrine Lighthouse on Kelvar Island was built in 1873 by engineer Matilda Orsk."},
    {"id": "c-ferry", "text": "The Kelvar Island ferry departs from Port Ashby twice daily, at 07:15 and 16:40."},
    {"id": "c-stout", "text": "Bluefin Brewery's flagship stout is aged for 19 months in cedar barrels."},
    {"id": "c-bridge", "text": "The Halvard suspension bridge has a main span of 1,288 metres."},
]
SHOWN = list(enumerate(CTX))
VAL = [
    {"id": "ctx-1", "text": "Paris is the capital of France and its largest city. The Seine river runs through it."},
    {"id": "ctx-2", "text": "The Earth orbits the Sun once every 365.25 days. This period is called a year."},
    {"id": "ctx-3", "text": "Water freezes at zero degrees Celsius at standard pressure."},
]


def ids(out):
    return [c["context_id"] for c in out["citations"]]


class GroundAnswerTests(unittest.TestCase):
    def test_marker_cites_only_used_context_and_is_stripped(self):
        out = service.ground_answer("It was built in 1873 by Matilda Orsk [1].", "Who built the Zephyrine Lighthouse?", SHOWN)
        self.assertFalse(out["abstained"])
        self.assertEqual(ids(out), ["c-light"])
        self.assertEqual(out["answer"], "It was built in 1873 by Matilda Orsk.")

    def test_multiple_markers_dedup(self):
        out = service.ground_answer(
            "The ferry leaves at 07:15 [2]; the stout is aged 19 months [3][2].",
            "When does the Kelvar Island ferry leave and how long is Bluefin stout aged?", SHOWN)
        self.assertEqual(ids(out), ["c-ferry", "c-stout"])

    def test_lexical_fallback_without_markers(self):
        out = service.ground_answer(
            "The Zephyrine Lighthouse was built in 1873 by engineer Matilda Orsk.",
            "Who built the Zephyrine Lighthouse, and in what year?", SHOWN)
        self.assertEqual(ids(out), ["c-light"])

    def test_out_of_range_marker_never_fabricates_ids(self):
        out = service.ground_answer("Built by Matilda Orsk in 1873 [9].", "Who built the Zephyrine Lighthouse?", SHOWN)
        self.assertTrue(set(ids(out)) <= {c["id"] for c in CTX})
        self.assertEqual(ids(out), ["c-light"])

    def test_refusal_phrasings_abstain(self):
        for text in (
            "I cannot answer this from the provided context.",
            "The provided documents do not contain information about the population of Zorblax-9.",
            "I'm sorry, there is no information about that in the context.",
        ):
            out = service.ground_answer(text, "What is the population of Zorblax-9?", SHOWN)
            self.assertTrue(out["abstained"], text)
            self.assertEqual(out["citations"], [], text)
            self.assertEqual(out["answer"], service.ABSTAIN_ANSWER)
            self.assertEqual(out["abstain_reason"], "refusal")

    def test_outside_knowledge_is_unsupported(self):
        # validator abstention case shape: only the Paris context is shown
        shown = [(0, VAL[0])]
        out = service.ground_answer("Water freezes at 32 degrees Fahrenheit.", "At what temperature does water freeze?", shown)
        self.assertTrue(out["abstained"])
        # The question coverage gate fires first
        # (nothing about water/freezing is in the Paris context).
        self.assertIn(out["abstain_reason"], ("unsupported", "question_not_covered"))

    def test_cross_chunk_confabulation_is_unsupported(self):
        out = service.ground_answer(
            "The Halvard suspension bridge was designed by engineer Matilda Orsk.",
            "Who designed the Halvard suspension bridge?", SHOWN)
        self.assertTrue(out["abstained"], out)

    def test_off_subject_restatement_is_unsupported(self):
        out = service.ground_answer(
            "The Zephyrine Lighthouse on Kelvar Island was built in 1873 by engineer Matilda Orsk.",
            "Which company manufactures the engines of the Kelvar Island ferry?", SHOWN)
        self.assertTrue(out["abstained"], out)

    def test_generic_summary_question_cites_all_used(self):
        out = service.ground_answer(
            "Paris is the capital of France. The Earth orbits the Sun every 365.25 days. Water freezes at zero degrees Celsius.",
            "Summarize the three provided facts.", list(enumerate(VAL)))
        self.assertFalse(out["abstained"])
        self.assertEqual(sorted(ids(out)), ["ctx-1", "ctx-2", "ctx-3"])

    def test_hedged_answer_with_marker_is_kept(self):
        out = service.ground_answer(
            "The context does not say who designed it, but it was built in 1873 [1].",
            "When was the Zephyrine Lighthouse built?", SHOWN)
        self.assertFalse(out["abstained"])
        self.assertEqual(ids(out), ["c-light"])

    def test_empty_answer_abstains(self):
        self.assertTrue(service.ground_answer("   ", "q?", SHOWN)["abstained"])

    def test_abstention_output_is_complete_pure_refusal(self):
        # The strict API grader matches complete clauses, not marker substrings.
        canonical = "I cannot answer from the provided context."
        self.assertEqual(service.ABSTAIN_ANSWER, canonical)
        for answer in ("", "I cannot answer this from the provided context."):
            with self.subTest(answer=answer):
                out = service.ground_answer(answer, "What is the population?", SHOWN)
                self.assertEqual(out["answer"], canonical)
                self.assertEqual(out["citations"], [])
                self.assertTrue(out["abstained"])

    def test_prompt_requests_same_pure_refusal(self):
        # The prompt asks for exactly the shared abstain answer and forbids outside knowledge.
        self.assertIn("exactly: " + service.ABSTAIN_ANSWER, service.SYSTEM_PROMPT)
        self.assertIn("Never use outside knowledge", service.SYSTEM_PROMPT)

    def test_numbers_with_commas_tokenize(self):
        out = service.ground_answer(
            "The Halvard suspension bridge has a main span of 1,288 metres.",
            "How long is the main span of the Halvard bridge?", SHOWN)
        self.assertEqual(ids(out), ["c-bridge"])


class SelectionAndPromptTests(unittest.TestCase):
    def test_select_keeps_all_when_under_limit(self):
        self.assertEqual(service.select_generation_contexts("q", CTX, limit=4), SHOWN)

    def test_select_reranks_and_limits(self):
        scorer = lambda q, texts: [0.1, 5.0, 3.0, -1.0]  # noqa: E731
        shown = service.select_generation_contexts("q", CTX, scorer=scorer, limit=2)
        self.assertEqual([i for i, _ in shown], [1, 2])

    def test_select_without_scorer_truncates_in_order(self):
        self.assertEqual([i for i, _ in service.select_generation_contexts("q", CTX, limit=2)], [0, 1])

    def test_messages_number_contexts_and_hide_ids(self):
        shown = [(2, CTX[2]), (0, CTX[0])]
        user = service.build_generation_messages("Who built it?", shown)[1]["content"]
        self.assertIn('[1] """Bluefin', user)
        self.assertIn('[2] """The Zephyrine', user)
        self.assertNotIn("c-light", user)

    def test_markers_map_to_shown_order_not_request_order(self):
        shown = [(2, CTX[2]), (0, CTX[0])]
        out = service.ground_answer("It was built in 1873 by Matilda Orsk [2].", "Who built the Zephyrine Lighthouse?", shown)
        self.assertEqual(ids(out), ["c-light"])

    def test_plain_prompt_without_chat_template(self):
        self.assertTrue(service.build_generation_prompt("q?", SHOWN, tokenizer=None).endswith("Answer:"))

    def test_chat_template_used_when_available(self):
        class Tok:
            chat_template = "x"

            def apply_chat_template(self, messages, tokenize, add_generation_prompt):
                assert tokenize is False and add_generation_prompt is True
                return "TEMPLATED:" + messages[1]["content"]

        self.assertTrue(service.build_generation_prompt("q?", SHOWN, Tok()).startswith("TEMPLATED:"))


class OpenAIBackendTests(unittest.TestCase):
    """Hosted backend mode: same /generate contract + grounding, end to end through Handler.

    Ported to the supported Chutes path (GEN-001 legacy test ruling): synthetic
    enabled admission (chutes_fixture) and a loopback provider stand-in that returns
    the complete current envelope (exact model, one choice, stop, integer usage).
    Only the model identity changed, to the fixture's explicit synthetic model.
    """

    def setUp(self):
        self.seen = []
        test = self

        class FakeOpenAI(BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                test.seen.append((self.path, body))
                user = body["messages"][1]["content"]
                text = ("It was built in 1873 by Matilda Orsk [1]." if "Question: Who built the Zephyrine" in user
                        else "I cannot answer this from the provided context.")
                out = json.dumps(completion(text, prompt_tokens=50, completion_tokens=12)).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)

            def log_message(self, *a):
                return

        self.fake = ThreadingHTTPServer(("127.0.0.1", 0), FakeOpenAI)
        threading.Thread(target=self.fake.serve_forever, daemon=True).start()
        self.svc = ThreadingHTTPServer(("127.0.0.1", 0), service.Handler)
        threading.Thread(target=self.svc.serve_forever, daemon=True).start()
        self.chutes = SyntheticChutes(self, self.fake.server_address[1])
        service.set_generate_impl_for_tests(None)

    def tearDown(self):
        self.fake.shutdown()
        self.fake.server_close()
        self.svc.shutdown()
        self.svc.server_close()

    def _gen(self, query):
        body = json.dumps({"query": query, "contexts": CTX[:2]}).encode()
        req = urllib.request.Request(f"http://127.0.0.1:{self.svc.server_address[1]}/generate", data=body,
                                     headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=5) as r:
            return json.loads(r.read())

    def test_grounded_answer_via_openai_backend(self):
        out = self._gen("Who built the Zephyrine Lighthouse?")
        self.assertEqual(out["answer"], "It was built in 1873 by Matilda Orsk.")
        self.assertEqual(out["citations"], [{"context_id": "c-light"}])
        self.assertFalse(out["abstained"])
        self.assertEqual(out["model"], SYNTHETIC_MODEL)
        self.assertEqual(out["usage"], {"prompt_tokens": 50, "completion_tokens": 12, "total_tokens": 62})
        path, body = self.seen[0]
        self.assertEqual(path, "/v1/chat/completions")
        self.assertEqual(body["temperature"], 0.0)
        self.assertIn('[1] """The Zephyrine', body["messages"][1]["content"])
        self.assertEqual(body["model"], SYNTHETIC_MODEL)
        self.assertEqual([m["role"] for m in body["messages"]], ["system", "user"])
        self.assertEqual(self.chutes.requested, [(CANONICAL_HOST, None)])
        self.assertEqual(self.chutes.pending(), [])  # completed and validated, no hold

    def test_abstention_via_openai_backend(self):
        out = self._gen("What is the population of Zorblax-9?")
        self.assertTrue(out["abstained"])
        self.assertEqual(out["citations"], [])
        self.assertEqual(len(self.seen), 1)
        self.assertEqual(self.chutes.pending(), [])


if __name__ == "__main__":
    unittest.main()
