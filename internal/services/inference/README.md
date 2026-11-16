# Inference service

This Python service runs the CPU embedding and reranking backends used by the supported miner deployment. It wraps `sentence-transformers`; the Go model proxy handles service registration and forwards requests from the miner.

The same image also runs the separate Chutes generation service. For a complete installation, use [SETUP.md](../../../SETUP.md). This page describes the backend for contributors and operators using custom deployments.

## Services

| Compose service | Role |
|---|---|
| `miner-inference-service` | CPU embedding and reranking on port `9200` |
| `cpu-model-proxy` | Registers those capabilities and forwards requests to the inference service |
| `chutes-generation` | Optional hosted generation on port `19201`; see [Chutes generation](../../../docs/GENERATION_CHUTES.md) |
| `chutes-genproxy` | Registers the generation capability and forwards to the Chutes service |

The CPU profile serves both embedding and reranking through `cpu-model-proxy`. The GPU profile uses vLLM for embedding and keeps reranking on this CPU service. These backend ports are private to the Compose network.

## Configuration

The image is built from `Dockerfile.inference-service` at the repository root.

| Service setting | Default | Compose input |
|---|---|---|
| `LISTEN_ADDR` | `:9200` | Set by the Compose service |
| `MODEL_DEVICE` | `cpu` | `MINER_MODEL_DEVICE` |
| `EMBED_MODEL` | `BAAI/bge-small-en-v1.5` | `MINER_EMBED_MODEL` |
| `RERANK_MODEL` | `cross-encoder/ms-marco-MiniLM-L-2-v2` | `MINER_RERANK_MODEL` |
| `EMBED_BATCH_SIZE` | `64` | `MINER_EMBED_BATCH_SIZE` |
| `RERANK_BATCH_SIZE` | `32` | `MINER_RERANK_BATCH_SIZE` |

The bundled image uses CPU PyTorch. Changing `MODEL_DEVICE` alone does not install a GPU runtime. See the [environment reference](../../../docs/ENVIRONMENT.md) for Compose configuration and model declarations.

## HTTP endpoints

These are backend endpoints. Public requests go through the miner's authenticated listener.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/health` | Service, configured models and device |
| `POST` | `/embed` | Embed one string or a list of strings |
| `POST` | `/rerank` | Score and rank candidate texts for a query |
| `GET` | `/health/generation` | Generation readiness; unavailable on the CPU inference service |
| `POST` | `/generate` | Hosted generation on the Chutes service; disabled on the CPU service |

Embed one text:

```json
{"inputs":"hello world"}
```

Embed a batch:

```json
{"inputs":["hello","world"]}
```

The response is a vector for a single input or a list of vectors for a batch. Vectors are normalized.

Rerank texts:

```json
{"query":"hello","texts":["hello world","unrelated text"]}
```

The response is sorted by descending score. Each entry includes its original `index` and `score`. It also includes `model` when the loaded model reports a repository identity, and `model_revision` when the loader reports a commit hash. A missing revision means unknown; it is not inferred from the cache or the configured model name.

## Running and testing

After configuring and starting the miner as described in SETUP.md, start these services from the repository root:

```bash
docker compose up -d --build miner-inference-service cpu-model-proxy
```

Run the Python unit tests from this directory:

```bash
python3 -m unittest discover -s . -p 'test_*.py' -v
```

Most tests stub model libraries. They check request handling, generation accounting and error behavior; they do not measure real-model quality or host capacity. Use the actual models and a representative workload for those checks.

## Generation

Generation is available only through the Chutes service. `GEN_OPENAI_URL` and a `GENERATION_CONNECTOR` other than `chutes` disable generation; they do not redirect it to another provider. Embedding and reranking continue to work.

The [Chutes reference](../../../docs/GENERATION_CHUTES.md) covers credentials, the persistent ledger, concurrency, deadlines and retries. `CHUTES_MAX_CONCURRENT` controls generation concurrency; the older `GEN_MAX_INFLIGHT`, `GEN_MAX_QUEUE` and `HOSTED_*` concurrency/recovery settings are ignored by that service.

Generation selects supplied contexts, builds the prompt and checks the answer's citations and support. These checks use lexical heuristics: they can reject a valid paraphrase or miss a contradiction expressed with similar words. They do not establish semantic entailment. The grounding implementation and tests are in `grounding.py` and `test_grounding.py`.
