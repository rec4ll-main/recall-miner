"""v2p6: /rerank reports the executing CrossEncoder's loaded identity in the shape the
deployed Go miner (rerank-identity-v1 internal/handlers/rerank.go) validates.

Runs the real service.Handler over loopback HTTP with a synthetic reranker instance;
no model load, no network beyond 127.0.0.1.
"""
import http.client, json, os, re, sys, threading, unittest
from http.server import ThreadingHTTPServer
from types import SimpleNamespace as NS

sys.path.insert(0, os.environ.get("CANDIDATE_DIR", os.path.dirname(os.path.abspath(__file__))))
import service  # noqa: E402

# Exactly the Go miner's checks (rerank.go).
GO_MODEL = re.compile(r'^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$')
GO_REVISION = re.compile(r'^[0-9a-f]{40}$')


def go_accepts(rows, n):
    """Mirror of rerankTexts: returns (scores, model, revision) or raises AssertionError."""
    assert isinstance(rows, list) and len(rows) == n, 'incomplete ranking'
    model, revision = rows[0].get('model', ''), rows[0].get('model_revision', '')
    if (model and not GO_MODEL.match(model)) or (revision and (not model or not GO_REVISION.match(revision))):
        raise AssertionError('invalid identity')
    scores, seen = [0.0] * n, [False] * n
    for r in rows:
        assert r.get('model', '') == model and r.get('model_revision', '') == revision, 'inconsistent identity'
        i = r.get('index')
        assert isinstance(i, int) and 0 <= i < n and not seen[i] and isinstance(r.get('score'), float), 'invalid ranking'
        seen[i] = True
        scores[i] = r['score']
    return scores, model, revision


class Fake:
    def __init__(self, name, revision=None, scores=(0.1, 0.9, 0.5)):
        self.model = NS(config=NS(_name_or_path=name, _commit_hash=revision))
        self._scores = list(scores)
        self.calls = 0

    def predict(self, pairs, batch_size=None):
        self.calls += 1
        return self._scores[:len(pairs)]


class RerankIdentity(unittest.TestCase):
    def setUp(self):
        self.old = service.reranker
        self.server = ThreadingHTTPServer(('127.0.0.1', 0), service.Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def tearDown(self):
        service.reranker = self.old
        self.server.shutdown()
        self.server.server_close()

    def post(self, path, body):
        c = http.client.HTTPConnection('127.0.0.1', self.server.server_address[1], timeout=10)
        data = json.dumps(body).encode()
        c.request('POST', path, body=data, headers={'Content-Type': 'application/json', 'Content-Length': str(len(data))})
        r = c.getresponse()
        out = (r.status, json.loads(r.read()))
        c.close()
        return out

    def rerank(self, fake, texts=('a', 'b', 'c')):
        service.reranker = fake
        status, rows = self.post('/rerank', {'query': 'q', 'texts': list(texts)})
        self.assertEqual(status, 200, rows)
        return rows

    def test_loaded_identity_and_revision_on_every_row(self):
        fake = Fake('cross-encoder/ms-marco-MiniLM-L-2-v2', 'a' * 40)
        rows = self.rerank(fake)
        self.assertEqual([r['index'] for r in rows], [1, 2, 0])
        scores, model, revision = go_accepts(rows, 3)
        self.assertEqual((model, revision), ('cross-encoder/ms-marco-MiniLM-L-2-v2', 'a' * 40))
        self.assertEqual(scores, [0.1, 0.9, 0.5])
        self.assertEqual(fake.calls, 1)

    def test_unknown_identity_is_legacy_shape(self):
        for name in (None, '', '/models/cache/reranker', '../private', 'https://tok@host/m', 'no-slash'):
            rows = self.rerank(Fake(name, 'b' * 40))
            self.assertTrue(all(set(r) == {'index', 'score'} for r in rows), (name, rows))
            self.assertEqual(go_accepts(rows, 3)[1:], ('', ''))

    def test_revision_never_guessed(self):
        for rev in (None, 'main', 'A' * 40, 'a' * 39):
            rows = self.rerank(Fake('cross-encoder/x', rev))
            self.assertTrue(all('model_revision' not in r for r in rows), rows)
            self.assertEqual(go_accepts(rows, 3)[1:], ('cross-encoder/x', ''))

    def test_identity_ignores_configured_env_name(self):
        # Identity comes from the executing instance, never from RERANK_MODEL.
        rows = self.rerank(Fake('cross-encoder/actually-loaded'))
        self.assertNotEqual(service.RERANK_MODEL, 'cross-encoder/actually-loaded')
        self.assertEqual(go_accepts(rows, 3)[1], 'cross-encoder/actually-loaded')

    def test_empty_texts(self):
        rows = self.rerank(Fake('cross-encoder/x'), texts=())
        self.assertEqual(rows, [])

    def test_load_failure_still_500(self):
        service.reranker = None
        old = service.CrossEncoder

        def boom(*a, **k):
            raise OSError('offline: no cached weights')
        service.CrossEncoder = boom
        try:
            status, body = self.post('/rerank', {'query': 'q', 'texts': ['a']})
        finally:
            service.CrossEncoder = old
        self.assertEqual(status, 500)
        self.assertIn('offline', body['detail'])

    def test_generation_fallback_unchanged_by_identity(self):
        # The /generate context selector still uses the plain scorer path (no identity).
        fake = Fake('cross-encoder/x', scores=(0.1, 0.9, 0.5, 0.2, 0.3))
        service.reranker = fake
        ctx = [{'id': str(i), 'text': 't%d' % i} for i in range(5)]
        old = service._RERANK_UNAVAILABLE
        try:
            service._RERANK_UNAVAILABLE = False
            shown = service._select_contexts('q', ctx)
        finally:
            service._RERANK_UNAVAILABLE = old
        self.assertEqual([i for i, _ in shown], [1, 2, 4, 3])


if __name__ == '__main__':
    unittest.main(verbosity=2)
