# Recall miner

Recall is Bittensor subnet 31 on finney. It provides vector storage, embeddings, reranking and retrieval-augmented generation through the [Recall API and console](https://rec4ll.com).

A miner runs these services on your infrastructure. The Go miner process exposes one HTTPS endpoint, authenticates requests from the Recall API and forwards them to the appropriate backend:

- **Vector DB**: Qdrant, through `miner-db-service`.
- **Embedding**: a CPU inference service, or vLLM on a GPU.
- **Rerank**: the CPU inference service.
- **Generation** (optional): the Chutes sidecar, which calls a hosted model on Chutes.

The supported deployment uses Docker Compose. The miner signs registration, heartbeats and responses with its Bittensor hotkey. Customers use the Recall API; they do not call miners directly.

## What gets scored

The Recall API checks four service tiers: vector DB (T1), embedding (T2), rerank (T3) and generation (T4). Validators use that evidence and verified customer traffic to allocate rewards. Registration and uptime alone do not establish eligibility. See [how scoring works](SETUP.md#9-how-scoring-works).

## Hardware at a glance

| Profile | CPU | RAM | Disk | GPU |
|---|---|---|---|---|
| CPU, minimum | 8 vCPU | 32 GB | 200 GB SSD | none |
| CPU, recommended | 16 vCPU | 64 GB | 500 GB NVMe | none |
| GPU | as CPU | as CPU | as CPU | 1 NVIDIA GPU |

These are deployment sizing guidelines, not an on-chain RAM check. Capacity depends on the models, stored data and concurrent requests. Use 64-bit Linux with Docker Engine and the Compose plugin. Generation runs on Chutes and needs no local GPU. The same sizing values are in [min_compute.yml](min_compute.yml).

## Get started

[Install a miner](SETUP.md) covers registration, configuration, TLS, startup and verification. Deploy a published release tag and use the guide from that tag; `main` can include changes that have not been released.

For repeatable deployments, use the [deployment checklist](setup-agents.md) alongside the install guide.

## Reference

| Task | Guide |
|---|---|
| Configure or tune a miner | [Environment reference](docs/ENVIRONMENT.md) |
| Add hosted generation | [Chutes generation](docs/GENERATION_CHUTES.md) |
| Update, back up or troubleshoot | [Operations](SETUP.md#10-operations) · [Troubleshooting](SETUP.md#12-troubleshooting) |
| Move to a new hotkey | [Fresh instance setup](docs/FRESH_START.md) |
| Work on a backend | [Inference service](internal/services/inference/README.md) · [Storage service](internal/services/storage/README.md) |
| Implement retention proofs | [Endpoint contract](docs/RETENTION_PROOF.md) |
| Report a vulnerability | [Security policy](SECURITY.md) |

The `cmd/` directory contains the miner, database service and model proxy entry points. Their Go packages and the Python inference service live under `internal/`.

## License

[MIT](LICENSE)
