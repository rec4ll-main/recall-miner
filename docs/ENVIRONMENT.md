# Miner environment reference

Use [SETUP.md](../SETUP.md) for installation. This page explains the deployment settings and which service receives them. It describes the code in this checkout; use the copy from your installed release.

Compose uses the root `.env` for substitution. It does not pass every variable to every container. A setting takes effect only when the Compose file forwards it, and an exported shell variable can override `.env`. Validate with `docker compose config --quiet`; the full rendered output contains secrets.

## Which URL means what?

| Setting | Purpose | Normal value |
|---|---|---|
| `RECALL_API_URL` | Remote API for miner registration and heartbeats | `https://rec4ll.com/api` |
| `MINER_API_URL` | This miner's advertised public listener | `https://<public-host>:6001` |
| `SIDECAR_MINER_API_URL` | Address used by local sidecars | `https://miner:8001` |
| `MINER_TLS_CA_FILE` | Certificate trusted by those sidecars | `/var/lib/recall-miner/certs/miner-cert.pem` |

Registration and heartbeat calls go to `/api/miner/register` and `/api/miner/heartbeat`, signed by the miner hotkey. They do not send the miner's internal API key.

An empty `RECALL_API_URL` uses the public default; it does not disable registration. A base without `/api` also works, and trailing slashes are removed. Set `MINER_API_URL` explicitly: registration sends the configured endpoint, without discovering or substituting a public address.

## Production versus mainnet

Set `ENVIRONMENT=production` for a public miner. This enables the checks for empty or placeholder secrets and unsupported database backends.

Only `dev` and `test` bypass those checks. Other values, including empty or unset, retain them. The parser lowercases but does not trim the value. Do not use a development value to work around a production startup error.

This setting does not choose a blockchain. The Go miner does not read `NETUID` or `NETWORK`, connect to Bittensor or register a hotkey on-chain. Register the hotkey separately on finney subnet 31 and use the intended Recall API.

## Keys and identities

| Setting | Purpose |
|---|---|
| `MINER_HOTKEY_URI` | Private sr25519 seed (`0x` plus 64 hex characters) or mnemonic. Compose passes it as `HOTKEY_URI`. A wallet name, file path or SS58 address is not a signing secret. |
| `API_SIGNING_HOTKEY_SS58` | Public identity allowed to send requests to this miner. The mainnet template uses `5ECDAred992gLh55nJ7PxQDtihHXy5qZZXuCkqLVJdmDsrYw`. Missing, empty or invalid values fail production startup. |
| `MINER_INTERNAL_API_KEY` | Random secret unique to this miner stack. Compose passes it as `INTERNAL_API_KEY` to the miner and authenticated sidecars. |

The Recall API's own internal credential is separate. A public miner does not need it. Protect your miner's internal key: it permits service registration and control operations, even though its name says “internal.” The setup guide writes a random 64-character hex value directly to `.env`, with mode `0600`.

Startup rejects empty internal keys and values containing known placeholder markers (`local-miner-internal-key`, `change-me`, `changeme`, `placeholder`, `local-dev`, `local-api`). This is a basic configuration check, not a measurement of key strength. Generate a random value and update the miner and sidecars together when rotating it.

Compatibility details for custom deployments:

- Compose accepts `HOTKEY_URI` when `MINER_HOTKEY_URI` is empty. Direct binaries try `HOTKEY_URI`, `HOTKEY_PHRASE`, `MINER_HOTKEY_URI`, then `MINER_HOTKEY_PHRASE`.
- `VALIDATOR_HOTKEY_SS58` and `APPLICATION_PROBER_HOTKEY_SS58` were removed. Their presence in the miner process environment, even with an empty value, fails startup. Delete them from old overrides and service definitions.
- `VALIDATOR_ALLOWLIST_DEV_EMPTY=true` permits an absent API signing identity only in development. A present but empty or invalid identity still fails.

## Public Compose settings

### Project and profiles

| Setting | Default or required value | Purpose |
|---|---|---|
| `COMPOSE_PROJECT_NAME` | Unique, stable name per miner | Scopes containers, networks and named volumes; preserve it on upgrades |
| `COMPOSE_FILE` | Root file plus `deploy/compose/cpu.yml` or `deploy/compose/gpu.yml` | Selects one supported profile; root alone starts overlapping CPU/GPU services |
| `COMPOSE_PROFILES` | Unset, or `chutes` | Adds hosted generation |
| `GIT_COMMIT` | Build default `dev` | Set with `GIT_COMMIT=$(git rev-parse --short=7 HEAD) docker compose build` to report the built commit |
| `VLLM_IMAGE` | Required by the GPU override | Operator-tested vLLM image pinned by digest |

### Listener and TLS

| Setting | Default | Purpose |
|---|---|---|
| `MINER_HOST_PORT` | `6001` | Host port mapped to container port `8001`; match the public endpoint and firewall |
| `TLS_ENABLED` | Runtime fallback `false`; production template `true` | Keep `true` for a public miner |
| `TLS_AUTO_GENERATE` | `true` | Creates or renews the native certificate when needed; `false` requires valid existing files |
| `TLS_CERT_FILE` | `/var/lib/recall-miner/certs/miner-cert.pem` | Certificate path |
| `TLS_KEY_FILE` | `/var/lib/recall-miner/certs/miner-key.pem` | Private TLS key path |
| `TLS_SAN_HOSTS` | `miner` | Extra certificate names; preserve `miner` for sidecar hostname verification |

Preserve the certificate volume. Sidecars load the trusted certificate at startup and need restarting after it changes. See [TLS setup](../SETUP.md#5-tls-and-the-public-endpoint).

### Storage and models

| Setting | Default | Purpose |
|---|---|---|
| `DB_ENGINE` | `qdrant` | Supported production backend |
| `STORAGE_PREFIX` | Empty | Distinct namespace for each miner sharing a Qdrant instance; [requirements](../internal/services/storage/README.md#several-miners-on-one-qdrant) |
| `MINER_EMBEDDING_MODELS` | Set explicitly | Catalog IDs actually served; use `bge-small-en-v1.5` for the supplied profiles |
| `MINER_EMBED_MODEL`, `VLLM_MODEL` | `BAAI/bge-small-en-v1.5` | Model loaded by CPU inference or vLLM |
| `MINER_RERANK_MODEL` | `cross-encoder/ms-marco-MiniLM-L-2-v2` | CPU reranker |
| `MINER_MODEL_DEVICE` | `cpu` | Inference device; does not select the GPU Compose profile |
| `HF_TOKEN` | Empty | Optional Hugging Face credential for vLLM downloads; keep it private |
| `NVIDIA_VISIBLE_DEVICES` | `all` | GPUs made available to vLLM |
| `GENERATION_ENABLED` | Runtime `false`, Compose fallback `true`, production template `false` | Keep explicitly `false` until Chutes is configured and verified |

Advertising a model does not install it. An empty catalog list advertises no catalog models. Changing the embedding model for existing data requires migration and re-embedding.

Unknown database engines fail startup. Experimental Milvus and PostgreSQL adapters require `dev`/`test` or an explicit unsupported-backend override; they are not supported production alternatives.

### Heartbeats and metrics

| Setting | Default | Container setting |
|---|---|---|
| `MINER_HEARTBEAT_INTERVAL_SECONDS` | `30` | Miner `HEARTBEAT_INTERVAL_SECONDS` |
| `SIDECAR_REGISTRATION_INTERVAL_SECONDS` | `10` | Sidecar registration/heartbeat interval |
| `MINER_HTTP_TIMEOUT_SECONDS` | `120` | Miner `HTTP_TIMEOUT_SECONDS` |
| `MINER_METRICS_STORAGE` | `duckdb` | Miner `METRICS_STORAGE`; also accepts `none` or `clickhouse` |

Use positive intervals. ClickHouse requires its connection settings in a custom deployment override.

### Request limits

| Setting | Default | Effect |
|---|---|---|
| `RATE_LIMIT_AUTH_FAILURES_PER_MIN` | `60` | Address-based failed-authentication budget; IPv6 addresses share a `/64` budget; `0` disables |
| `RATE_LIMIT_VALIDATOR_PER_MIN` | `3000` | Authenticated requests per signer per minute; excess requests receive `503`; `0` disables |
| `RATE_LIMIT_DB_CREATE_PER_MIN` | `20` | Database creates per signer per minute; excess requests receive `503`; `0` disables |
| `RATE_LIMIT_AUTH_FAILURE_TABLE_SIZE` | `10000` | Maximum tracked client addresses; nonpositive values use the default |
| `RATE_LIMIT_AUTH_FAILURE_OVERFLOW_PER_MIN` | `600` | Shared budget for untracked addresses when the table is full; `0` refuses those requests |
| `RATE_LIMIT_TRUSTED_PROXIES` | Empty | Trusted proxy IPs/CIDRs for client-address accounting |
| `EMBED_MAX_INFLIGHT` | `64` | Concurrent embedding requests |
| `RERANK_MAX_INFLIGHT` | `32` | Concurrent rerank requests |
| `QUERY_MAX_INFLIGHT` | `128` | Concurrent query requests |
| `STORE_MAX_INFLIGHT` | `128` | Concurrent store requests |
| `GENERATE_MAX_INFLIGHT` | `8` | Concurrent generation requests |

A full route returns `503` with `Retry-After: 1`; `0` disables that route's concurrency cap. All Recall API traffic uses one signer, so size the authenticated limit for customer traffic as well as probes.

The address guard reserves capacity before authentication and refunds successful requests. Retained authentication failures can produce `429`; capacity tied up by pending work or the overflow budget produces `503`. Requests declaring the configured Recall API signer bypass this address guard but still undergo signature verification.

Keep `RATE_LIMIT_TRUSTED_PROXIES` empty for the direct native-TLS setup. With a trusted proxy, accounting uses the right-most untrusted `X-Forwarded-For` hop; malformed forwarding data is attributed to the proxy. Invalid proxy entries fail startup.

Use literal `true` or `false` for booleans. Invalid booleans and integer text fall back to runtime defaults, so verify changes rather than relying on a typo to disable a feature.

## Direct-process and deployment-override settings

These require changes to the relevant service's environment in a Compose override, not just additions to `.env`:

- `MAX_BODY_BYTES`, `SERVER_READ_TIMEOUT_SECONDS`, `SERVER_WRITE_TIMEOUT_SECONDS`, `SERVER_IDLE_TIMEOUT_SECONDS`, `MINER_REGISTRATION_INTERVAL_SECONDS` and `ALLOW_UNSUPPORTED_DB_ENGINE`.
- `EPISTULA_ENVELOPE_REQUIRED` and `RESPONSE_SIGNING_ENABLED`. Keep authentication and signed responses enabled publicly.
- Proxy `CAPABILITIES` and `MODELS`. The CPU profile advertises embedding and reranking; the GPU profile gives embedding to the GPU proxy and reranking to the CPU proxy.
- Chutes timeout controls listed in [the generation reference](GENERATION_CHUTES.md#deadlines-how-a-slow-completion-surfaces).

Sidecar `REGISTER_ON_START` is fixed to `true` in Compose. Setting that name in `.env` does not stop sidecar registration or the miner's API registration loop. The `MINER_EXTERNAL_IP` and `MINER_ADVERTISE_*` helpers do not replace `MINER_API_URL` in the current startup path.

## Generation backend

Use [the Chutes setup](GENERATION_CHUTES.md). `CHUTES_KEY_DIR` holds the credential file. `CHUTES_STATE_DIR` optionally replaces the named ledger volume, and `CHUTES_LEDGER_ID` optionally pins the ledger identity. Preserve existing ledger storage when changing either setting.

`GEN_OPENAI_URL` and connectors other than `chutes` disable generation. `MINER_GEN_MODEL` does not enable a local backend. `GEN_OPENAI_TIMEOUT_SECONDS` in `.env` reaches only the CPU inference service in stock Compose, not the Chutes service.

## Advanced settings

Apply changes with `docker compose up -d`, which recreates affected containers. Then repeat the [verification checks](../SETUP.md#8-verify).

### Generation model (`CHUTES_MODEL`)

Set `CHUTES_MODEL` explicitly to the exact provider `org/name` ID. The template uses `Qwen/Qwen3.8-27B-TEE`. Check current provider capacity before changing it.

Compose sends the value to the miner, the sidecar and the generation proxy. They must agree. If it is unset or empty, the miner falls back to its code default, but the sidecar and proxy receive empty values. That combination does not serve generation; it reports `hosted configuration invalid`.

### Tuning

| Setting | Default | Reaches | Purpose |
|---|---|---|---|
| `GENERATION_BACKEND_TIMEOUT_SECONDS` | `58` | Miner | Generation deadline; the default fits the API's 60-second scoring window |
| `GENERATE_MAX_INFLIGHT` | `8` | Miner | Concurrent generation requests accepted |
| `CHUTES_MAX_CONCURRENT` | `4` | Chutes service | Provider concurrency, allowed `1`–`16`; up to twice as many requests can wait |
| `MINER_EMBED_BATCH_SIZE` | `64` | CPU inference | Embedding batch size |
| `MINER_RERANK_BATCH_SIZE` | `32` | CPU inference | Rerank batch size |
| `VLLM_GPU_MEMORY_UTILIZATION` | `0.12` | vLLM | GPU memory share |
| `VLLM_MAX_MODEL_LEN` | `512` | vLLM | Model context length |

Choose concurrency limits from measured host and provider capacity. If `CHUTES_MAX_CONCURRENT` exceeds 8, account for the miner's `GENERATE_MAX_INFLIGHT` limit too.

The Chutes service ignores `GEN_MAX_INFLIGHT`, `GEN_MAX_QUEUE`, `HOSTED_MAX_OUTSTANDING`, `HOSTED_MAX_CONCURRENT`, `HOSTED_RECOVERY_POLICY` and `HOSTED_UNKNOWN_AGE_OUT_SECONDS`, logging `setting_ignored`. Use `CHUTES_MAX_CONCURRENT`. Unknown provider attempts stop counting against admission after five minutes; their records remain in the ledger.
