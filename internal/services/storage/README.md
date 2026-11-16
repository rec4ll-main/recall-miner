# Storage service

The database sidecar stores and searches vectors for the miner. Public requests reach it through the miner; the sidecar authenticates those internal calls with the stack's service key.

`cmd/db-service` is the entry point. This directory contains the HTTP handlers, configuration, registry client and storage adapters.

## Service

The vector DB service is built from `Dockerfile` at the repo root with `TARGET=./cmd/db-service`. In Docker Compose it runs as `miner-db-service` on `:9000`.

Supported backends are selected with `DB_ENGINE`:

| Engine | Adapter | Primary Env | Support |
| --- | --- | --- | --- |
| `qdrant` | `qdrant.NewClient` | `QDRANT_URL`, `QDRANT_API_KEY` | **Mainnet v1 (only supported engine)** |
| `pgsql`, `postgres`, `postgresql`, `pgvector` | `pgsql.NewClient` | `PGSQL_DSN` or `DATABASE_URL` | Experimental; dev/test only |
| `milvus` | `milvus.NewClient` | `MILVUS_URI`, `MILVUS_TOKEN` | Experimental; dev/test only |

`DB_ENGINE` defaults to `qdrant`. An unknown engine is a startup error — it
never silently falls back to Milvus. In any non-dev environment (unset
`ENVIRONMENT`, `mainnet`, `production`, ...), the experimental
`milvus` and `pgsql` engines are rejected unless the explicit
`ALLOW_UNSUPPORTED_DB_ENGINE=true` allowlist flag is set. Only `dev` and
`test` permit them without the flag.

### Several miners on one Qdrant

Each miner's db service must have storage of its own. The Recall API manages database placement and replacement. If two miners share a storage namespace, a delete or restore for one miner can change the other miner's copy too.

- Give each miner its own Qdrant, **or** set `STORAGE_PREFIX` to a different value on every miner
  that shares one Qdrant (lowercase letters, digits and hyphens, up to 32 characters; Qdrant only).
  Each miner then stores its databases under `<prefix>--<db id>` and sees only its own.
- The db service records which service uses its storage (under its prefix). A second db service
  with the same prefix on the same Qdrant refuses to start, and a running one stops if it finds
  one, with an error naming the other service. A claim nobody refreshed for 3 minutes (a service
  that is gone) does not block.
- Changing `STORAGE_PREFIX` on a running miner leaves its databases under the old name: the
  existing data is no longer visible to that miner. Plan a migration or coordinate recovery with the Recall operator before changing it; do not delete the old data until recovery is verified.

All endpoints except `GET /health` require `X-Internal-Api-Key` matching `INTERNAL_API_KEY`.

### Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Service health and backend summary |
| `GET` | `/stats` | Collection and vector counts |
| `POST` | `/db/create` | Create a collection with explicit positive `dimension` |
| `GET` | `/db/{db_id}` | Get collection info |
| `DELETE` | `/db/{db_id}` | Drop a collection |
| `POST` | `/db/{db_id}/restore` | Recreate a collection from raw vectors |
| `POST` | `/db/{db_id}/store` | Store one vector and optional metadata |
| `DELETE` | `/db/{db_id}/embed/{embed_id}` | Delete one stored vector |
| `POST` | `/db/{db_id}/query` | Search by vector with optional `top_k` |

### Payloads

The following examples use a three-dimensional collection. Production vectors must match the dimension of the chosen embedding model.

Create collection:

```json
{"name":"docs","dimension":3}
```

Store vector:

```json
{"vector":[0.1,0.2,0.3],"metadata":{"source":"example"}}
```

Query collection:

```json
{"vector":[0.1,0.2,0.3],"top_k":10}
```

The DB service only accepts vectors. Text embedding happens before this service, either in the miner request path or through the API store path. Local CPU embedding/rerank inference lives in `internal/services/inference`.

## Compose services

The supported deployment uses these services:

| Compose Service | Role |
| --- | --- |
| `miner-db-service` | Vector DB sidecar registered with miner capability `db` |
| `qdrant` | Default vector backend (the only mainnet v1 engine) |
| `miner-inference-service` | CPU embedding/rerank implementation from `internal/services/inference` |
| `cpu-model-proxy` | Registers `embedding,rerank` capabilities and forwards to `miner-inference-service` |

After configuring the stack and starting the miner to create its TLS certificate ([SETUP.md](../../../SETUP.md#7-start)), start the storage services from the repository root:

```sh
docker compose up -d --build qdrant miner-db-service
```

See [the retention-proof contract](../../../docs/RETENTION_PROOF.md) for storage verification requests and [the environment reference](../../../docs/ENVIRONMENT.md) for backend configuration.
