# Generation with Chutes

Recall's generation tier (T4) uses a hosted Chutes model. It does not need a local GPU. The CPU and GPU profiles continue to serve storage, embedding and reranking independently.

Follow [SETUP.md, section 6](../SETUP.md#6-optional-generation-with-chutes) to install it. This reference covers operation and recovery. Use the documentation from your deployed release: ledger initialization and retry behavior can differ between releases.

## Shape

The optional Compose profile `chutes` adds two private services:

| Service | Role |
|---|---|
| `chutes-generation` | Calls `https://llm.chutes.ai/v1` and records provider attempts in a persistent ledger |
| `chutes-genproxy` | Registers the `generate` capability and forwards requests to the sidecar on port `19201` |

Both run as UID/GID `1001:1001`, with a read-only root filesystem, dropped capabilities and no published ports. The generation proxy checks `/health/generation` before advertising readiness.

Set `COMPOSE_PROFILES=chutes` in `.env`, alongside the CPU or GPU `COMPOSE_FILE` selection from SETUP.md. Generation is disabled when `GEN_OPENAI_URL` is set or `GENERATION_CONNECTOR` is not `chutes`; arbitrary OpenAI-compatible endpoints and local generation are unsupported.

## One-time setup (per miner)

| Setting in `.env` | Purpose |
|---|---|
| `CHUTES_MODEL` | Exact provider model ID, such as the template default `Qwen/Qwen3.8-27B-TEE`; aliases and pools are refused |
| `CHUTES_KEY_DIR` | Existing host directory containing `api-key`; mounted read-only at `/run/chutes` |
| `CHUTES_MAX_CONCURRENT` | Concurrent provider requests, default `4`, allowed `1`–`16` |
| `CHUTES_STATE_DIR` | Optional absolute host path instead of the default `chutes-state` volume |
| `CHUTES_LEDGER_ID` | Optional UUID that pins the existing ledger identity |

Compose passes `CHUTES_MODEL` to the miner, proxy and sidecar. Set it explicitly: an empty value prevents generation. A model's availability and rate limits depend on Chutes; check current capacity with your account.

### Credential file

Use the [key-file commands](../SETUP.md#62-key-file) to create `api-key` outside the checkout. The sidecar (UID 1001) must be able to read it, and it refuses a file that group or others can write; mode `0644` works. The key directory must exist before Compose starts. A root-owned `0600` file is unreadable by UID 1001.

Use a separate credential for each miner. Keep the key out of command-line arguments and logs. The sidecar reads it at startup, so restart `chutes-generation` after rotating it. Do not set `CHUTES_API_KEY`.

### Persistent ledger

By default, Compose mounts the named volume `chutes-state` at `/state`. The image supplies its ownership for UID 1001. On first start the sidecar creates `/state/hosted.sqlite`; later starts open the same ledger. No manual initialization is required.

For a host bind mount, create `CHUTES_STATE_DIR` with owner `1001:1001` and mode `0750` before starting. Preserve that directory when changing configuration.

Setting `CHUTES_LEDGER_ID` to the ledger's UUID makes a missing or different ledger an error. Without a pin, a deleted ledger is recreated empty at the next start and loses the record of unresolved provider requests. Keep the ledger on persistent storage; do not delete it, share it between miners or restore an older copy to clear an error. The service detects replacement or rollback while running, but cannot reliably detect an older copy restored across a restart.

## Verify before advertising

With `GENERATION_ENABLED=false`, start the configured stack and check the sidecar:

```bash
docker compose config --quiet
docker compose up -d
docker compose exec -T chutes-generation python -c "
import urllib.request, urllib.error
try:
    r = urllib.request.urlopen('http://127.0.0.1:19201/health/generation', timeout=5)
    print(r.status, r.read().decode())
except urllib.error.HTTPError as e:
    print(e.code, e.read().decode())"
```

A healthy response is HTTP `200` with `status: ok`, `connector: chutes` and the configured model. This checks local configuration and admission capacity; it does not make a provider request or prove that Chutes can currently serve the model. Follow the provider test in SETUP.md before enabling `GENERATION_ENABLED`.

A `503` includes a reason:

| Reason | What to check |
|---|---|
| `generation disabled` | Connector selection and `GEN_CHUTES_ENABLED`; the stock profile enables it |
| `hosted configuration invalid` | Exact model ID and credential contents |
| `hosted ledger directory missing; refusing to create it` | The `/state` mount |
| `hosted ledger missing; refusing to create a new one` | A ledger ID is pinned, but the expected file is missing |
| `hosted ledger identity mismatch` | The mount or configured UUID points to a different ledger; do not bypass the pin to clear the error |
| `hosted ledger or journal directory not writable by runtime UID` | Ownership and permissions on the existing ledger directory |
| `CHUTES_LEDGER_ID must be a ledger id (UUID) or left empty` | Invalid UUID configuration |
| `CHUTES_MAX_CONCURRENT must be a whole number from 1 to 16` | Concurrency setting |
| `hosted deadline unavailable` | Invalid timeout in a custom override |
| `GEN_ACQUIRE_TIMEOUT_SECONDS must be a number of seconds` | Invalid queue timeout in a custom override |
| `hosted request in progress` | All active provider slots are occupied; readiness returns when one is available |
| `unresolved hosted attempt held` | The unresolved-attempt limit is reached; see [recovery](#failure-behaviour-by-design) |
| Other `hosted ledger …` reasons | Unreadable, damaged or replaced storage; preserve it for diagnosis |

Readiness requires a free active slot and permits up to two recent attempts with unknown outcomes. The generation proxy withdraws the capability while this check is unavailable.

## Provider errors

Inspect the provider's status and outcome in the sidecar log:

```bash
docker compose logs --tail 200 chutes-generation | grep generation_transport_v1
```

| Result | Action |
|---|---|
| HTTP `429` | Check account limits and model capacity; reduce concurrency or choose an available model. The sidecar does not retry provider HTTP errors. |
| HTTP `401` | Check or rotate the key, then restart the sidecar |
| HTTP `402` | Check provider billing or balance |
| HTTP `5xx` | Check provider status; repeated failures can require a different model |
| `completed_unaccepted` | The response failed local validation, for example a missing or mismatched model identity or missing final content |

A complete provider HTTP error other than `408` is recorded as terminal. It fails that request but does not, by itself, make the local health check unavailable. HTTP `408`, a truncated reply or a lost connection after dispatch can leave an unknown outcome in the ledger. Failed requests still affect the miner's service results even if its sidecar remains healthy.

## Deadlines (how a slow completion surfaces)

The default miner budget is 58 seconds (`GENERATION_BACKEND_TIMEOUT_SECONDS`). It forwards the remaining budget minus a 2-second margin to the sidecar. The sidecar uses the smaller of that budget and its own 58-second limit (`GEN_OPENAI_TIMEOUT_SECONDS`). A normal request therefore has about 56 seconds for hosted generation, including queueing and retries.

The Recall API scores a generation answer only if it arrives within 60 seconds (it waits up to 90). The 58-second default keeps every answer, including one from a retry, inside that window with 2 seconds for the network. Raising it lets slow answers through to customers, but answers after 60 seconds earn nothing on probes.

The first provider attempt has a 25-second limit. A stalled or disconnected attempt can be followed by one new attempt when at least 15 seconds remain. Both attempts share the original request deadline. Requests are not streamed and use the provider's default output limit; a slow model can exhaust the budget without returning headers.

The stock Compose profile does not forward `GEN_OPENAI_TIMEOUT_SECONDS`, `GEN_ATTEMPT_TIMEOUT_SECONDS` or `GEN_ACQUIRE_TIMEOUT_SECONDS` to `chutes-generation`. Changing them requires an explicit Compose override; adding them to `.env` alone does not tune hosted generation.

## Concurrency

With `CHUTES_MAX_CONCURRENT=N`, the sidecar permits N active generation requests and up to 2×N waiters. Waiters have a 25-second queue limit by default. A full queue returns `503 generation_overloaded`.

The miner separately limits generation requests with `GENERATE_MAX_INFLIGHT` (default `8`). If you increase provider concurrency above 8, account for this miner limit too. Raise limits only when the host and provider can sustain the extra requests.

The older `GEN_MAX_INFLIGHT`, `GEN_MAX_QUEUE`, `HOSTED_MAX_OUTSTANDING`, `HOSTED_MAX_CONCURRENT`, `HOSTED_RECOVERY_POLICY` and `HOSTED_UNKNOWN_AGE_OUT_SECONDS` settings are ignored by the Chutes sidecar and produce a `setting_ignored` warning. Use `CHUTES_MAX_CONCURRENT`.

## Failure behaviour (by design)

Every provider attempt is recorded before dispatch. If its outcome is unknown, its ledger row stays `pending` as a record of possible spend.

After a stalled or disconnected attempt, the sidecar may send one new attempt within the original deadline. The first attempt remains pending; both may incur provider charges. Provider HTTP errors, including `429`, are not retried. Requests refused before dispatch send nothing to Chutes.

A caller that disconnects after an attempt is sent (a dropped API connection, or a restart of the miner process) does not stop that attempt. The sidecar reads the reply to the end, or until the request deadline, so the outcome is recorded instead of left unknown. A caller that disconnects before the send sends nothing.

Up to two recent unknown outcomes permit new requests. A third blocks new requests until enough earlier attempts stop counting. The one permitted retry excludes its own stalled first attempt from that count. Each unknown attempt ages out of the admission count after five minutes, but its row remains in the ledger. Aging out does not establish whether the provider completed or billed it.

Inspect the ledger through the service's own mount:

```bash
docker compose --profile chutes run --rm --no-deps -u 1001:1001 chutes-generation \
  python hosted_accounting.py status /state/hosted.sqlite
```

To mark an old attempt as reconciled with an unknown outcome:

```bash
docker compose --profile chutes run --rm --no-deps -u 1001:1001 chutes-generation \
  python hosted_accounting.py reconcile /state/hosted.sqlite --attempt <attempt-id>
```

The command only accepts attempts at least 300 seconds old. It preserves the attempt and request key, does not resend the request, and records `unknown` rather than a successful provider outcome. You can select by `--request-key` or `--all-pending-older-than` instead. Check provider usage records when reconciling possible spend. Do not edit the SQLite file or reconcile against a restored older copy.

## Rollback

To stop serving generation, set `GENERATION_ENABLED=false` in `.env` and run `docker compose up -d`. Then stop `chutes-generation` and `chutes-genproxy` and remove `chutes` from `COMPOSE_PROFILES` if they are no longer needed. Keep the ledger and credential file.

`GEN_CHUTES_ENABLED` is fixed to `true` by the stock profile, so setting it in `.env` does not disable generation. Storage, embedding and reranking continue independently.
