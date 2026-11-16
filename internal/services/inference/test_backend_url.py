"""Backend URL composition.

The GPU deploy proposal sets GEN_OPENAI_URL=http://127.0.0.1:6011/v1/chat/completions.
service.generate_via_openai appended "/chat/completions" unconditionally, so the
request went to /v1/chat/completions/chat/completions (404 on vLLM). Base + path
composition must be idempotent for bare, trailing-slash and already-suffixed
bases, and must keep any query string.

The wire test is ported to the supported Chutes path (GEN-001 legacy test ruling):
synthetic enabled admission (chutes_fixture), canonical provider base URL and a
loopback socket standing in for the provider. Query preservation in the URL helper
does not authorize arbitrary provider URLs: a non-canonical configured base URL and
a disabled connector are both refused before any connection is made.
"""

from __future__ import annotations

import json
import sys
import threading
import types
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.modules.setdefault("sentence_transformers", types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object))
sys.modules.setdefault("transformers", types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object))

import service  # noqa: E402
from chutes_fixture import CANONICAL_HOST, SyntheticChutes, completion  # noqa: E402
from hosted_provider import ChutesConfig, HostedProviderError  # noqa: E402


class _Recorder:
    def __init__(self):
        self.paths = []
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length") or 0))
                outer.paths.append(self.path)
                body = json.dumps(completion("ok [1]", prompt_tokens=1, completion_tokens=1)).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                return

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.root = f"http://127.0.0.1:{self.srv.server_address[1]}"

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class BackendURLTests(unittest.TestCase):
    BASES = [
        "/v1",
        "/v1/",
        "/v1/chat/completions",
        "/v1/chat/completions/",
        "/v1//",
    ]

    def test_chat_completions_url_is_idempotent(self):
        for suffix in self.BASES:
            got = service.chat_completions_url("http://127.0.0.1:6011" + suffix)
            self.assertEqual(got, "http://127.0.0.1:6011/v1/chat/completions", suffix)

    def test_query_string_is_preserved(self):
        self.assertEqual(service.chat_completions_url("http://h:1/v1/chat/completions?api-version=2"),
                         "http://h:1/v1/chat/completions?api-version=2")
        self.assertEqual(service.chat_completions_url("http://h:1/v1?api-version=2"),
                         "http://h:1/v1/chat/completions?api-version=2")

    def test_request_path_on_the_wire(self):
        rec = _Recorder()
        self.addCleanup(rec.close)
        chutes = SyntheticChutes(self, rec.srv.server_address[1])
        for suffix in self.BASES:
            service.GEN_OPENAI_URL = "https://" + CANONICAL_HOST + suffix
            text, _, _ = service.generate_via_openai([{"role": "user", "content": "q"}], 8, 0)
            self.assertEqual(text, "ok [1]")
        self.assertEqual(rec.paths, ["/v1/chat/completions"] * len(self.BASES), rec.paths)
        self.assertEqual(chutes.requested, [(CANONICAL_HOST, None)] * len(self.BASES))
        self.assertEqual(len(chutes.connections), len(self.BASES))  # one send per call
        self.assertEqual(chutes.pending(), [])                      # each completed and validated

    def test_refused_connectors_make_no_connection(self):
        rec = _Recorder()
        self.addCleanup(rec.close)
        chutes = SyntheticChutes(self, rec.srv.server_address[1])
        for config in (ChutesConfig(),
                       ChutesConfig(True, service.CHUTES_CONFIG.model, "synthetic-test-key",
                                    base_url=rec.root + "/v1"),
                       ChutesConfig(True, service.CHUTES_CONFIG.model, "synthetic-test-key",
                                    base_url="https://" + CANONICAL_HOST + "/v1?token=x")):
            service.CHUTES_CONFIG = config
            with self.subTest(config=config), self.assertRaises(HostedProviderError):
                service.generate_via_openai([{"role": "user", "content": "q"}], 8, 0)
        self.assertEqual((chutes.connections, rec.paths, chutes.pending()), ([], [], []))


if __name__ == "__main__":
    unittest.main()
