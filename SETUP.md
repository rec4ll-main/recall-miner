# Recall miner setup

Install a Recall miner on finney subnet 31, then verify registration, connectivity and backend health. Commands assume Bash, `sudo` and access to Docker. Run them from the miner checkout unless a step says otherwise.

Use this guide from the release you install. The copy on `main` may describe changes that are not in the latest published release. Passing setup checks does not establish probe eligibility or rewards.

For configuration details, see the [environment reference](docs/ENVIRONMENT.md). The [deployment checklist](setup-agents.md) is a short companion for repeat installations and handover.

## Contents

0. [Overview](#0-overview)
1. [Requirements](#1-requirements)
2. [Wallet and chain registration](#2-wallet-and-chain-registration)
3. [Get the code](#3-get-the-code)
4. [Configure .env](#4-configure-env)
5. [TLS and the public endpoint](#5-tls-and-the-public-endpoint)
6. [Optional: generation with Chutes](#6-optional-generation-with-chutes)
7. [Start](#7-start)
8. [Verify](#8-verify)
9. [How scoring works](#9-how-scoring-works)
10. [Operations](#10-operations)
11. [Advanced settings](#11-advanced-settings)
12. [Troubleshooting](#12-troubleshooting)
13. [FAQ](#13-faq)

## 0. Overview

### What you are running

```
                 Recall API prober (rec4ll.com)
                              |
          Epistula-signed HTTPS, pinned to your TLS key
                              v
host :6001 --> miner (Go, :8001, native TLS)       the only published port
                              |
      sidecars register with https://miner:8001 (Compose network only)
                              |
  +------------------+--------+---------+------------------+
  |                  |                  |                  |
miner-db-service  cpu-model-proxy    model-proxy        chutes-genproxy
  |                  |               (GPU profile)      (profile chutes)
qdrant           miner-inference-      |                  |
(vector DB)      service (CPU)       vllm (GPU)         chutes-generation --> llm.chutes.ai
                 embedding, rerank   embedding          generate
```

- `miner` is the only service with a host port. It signs its registration and heartbeats with your hotkey and serves the prober's signed requests.
- Each sidecar registers its capability with the miner. The miner advertises a capability only while a healthy sidecar serves it.
- Qdrant, the inference service, vLLM and the Chutes sidecar are reachable only on the Compose network.

### What is probed and scored

The Recall API sends test requests, called probes, to check the capabilities each miner serves:

| Tier | Capability | Served by | What the prober checks |
|---|---|---|---|
| T1 vector DB | `db` | Qdrant through `miner-db-service` | Stores and queries vectors, then checks integrity and retention |
| T2 embedding | `embedding` | CPU inference service, or vLLM on the GPU profile | Embedding quality on a fixed model |
| T3 rerank | `rerank` | CPU inference service | Rerank quality |
| T4 generation | `generate` | Chutes sidecar | Answers, citations, abstention and prompt-injection handling |

### Choose your capabilities

| Choice | Compose files | Advertises | Needs |
|---|---|---|---|
| CPU | `docker-compose.yml` + `deploy/compose/cpu.yml` | `db`, `embedding`, `rerank` | A CPU server |
| GPU | `docker-compose.yml` + `deploy/compose/gpu.yml` | `db`, `embedding` (vLLM), `rerank` (CPU) | An NVIDIA GPU and a vLLM image you pin by digest |
| Generation, on top of either | add the Compose profile `chutes` | `generate` as well | A Chutes account and API key |

Pick CPU or GPU, then decide whether to add generation. Always run Compose with the root file plus exactly one override. The root file alone starts the GPU services and advertises overlapping capabilities. Step 4 sets this up once in `.env`, so plain `docker compose` commands do the right thing.

Other combinations, such as a DB-only miner, need your own Compose override and are not covered here.

## 1. Requirements

### Hardware

| Profile | CPU | RAM | Disk | GPU |
|---|---|---|---|---|
| CPU, minimum | 8 vCPU | 32 GB | 200 GB SSD | none |
| CPU, recommended | 16 vCPU | 64 GB | 500 GB NVMe | none |
| GPU | as CPU | as CPU | as CPU | 1 NVIDIA GPU |

These figures are sizing guidance, also published in [min_compute.yml](min_compute.yml). The software does not enforce a 32 GB RAM admission threshold. Measure memory use, latency and throughput with your intended models and database size, especially on a smaller host.

Generation runs on Chutes, so it does not need a local GPU. Its request ledger needs persistent storage.

### Software

- 64-bit Linux.
- Docker Engine with the Compose v2 plugin (`docker compose`).
- `git`, `curl`, `openssl` and `python3` on the host.
- GPU profile only: an NVIDIA driver and the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html). Step 7 checks that containers can see the GPU.

Access to the Docker daemon is root-equivalent. Use a dedicated administrative account.

### Network

- **One inbound TCP port**, by default `6001` (`MINER_HOST_PORT`). It must be reachable from the internet. Open it in your cloud firewall or security group. Docker publishes ports through its own firewall rules, which can bypass ordinary `ufw` rules. Verify the effective rules and reachability; do not rely on `ufw` status alone. The stack publishes no other port.
- **A public address.** The Recall API rejects private miner endpoints. `MINER_API_URL` must be a public `https://` address that reaches this host.
- **Outbound HTTPS** to `rec4ll.com` (registration and heartbeats) and Hugging Face (model downloads on first start). With generation, also `llm.chutes.ai`. While building, also Docker Hub, PyPI, `download.pytorch.org` and the Go module proxy.

### Do you need a domain?

No. The miner creates a self-signed certificate, and the Recall API pins its public key instead of checking a certificate authority (step 5). `MINER_API_URL` can use a public IP address (`https://203.0.113.10:6001`) or a DNS name that resolves to the host. You do not need a CA-issued certificate.

Do not put a TLS-terminating proxy or CDN in front of the miner. It presents a different key, and registration fails. Plain TCP forwarding or passthrough is fine.

## 2. Wallet and chain registration

> **One hotkey per coldkey.** Subnet 31 scores one miner hotkey per coldkey. When a coldkey has several registered hotkeys, validators give weight only to the one registered most recently (on equal registration blocks, the lowest UID); the others earn nothing, even if they run and pass probes. Register each miner you run from its own coldkey.

Create a hotkey for this miner in your wallet (the default `sr25519` key without a password; the miner reads the unencrypted hotkey seed) and register it on subnet 31. `<wallet>` and `<hotkey>` are the local wallet and hotkey **names**, the directory and file names under `~/.bittensor/wallets`, not addresses:

```bash
btcli subnet register --netuid 31 --wallet-name <wallet> --hotkey <hotkey> --network finney
btcli subnet metagraph --netuid 31 --network finney
```

Checked with btcli 9.23.2; run `btcli subnet register --help` if your version differs.

Registration pays the chain's current cost from your coldkey; review the cost, coldkey, hotkey and subnet before you confirm. The metagraph must list your hotkey with a UID.

### Your hotkey on the miner server

This guide assumes the hotkey is already on the miner server at `~/.bittensor/wallets/<wallet>/hotkeys/<hotkey>`. The miner signs with it: step 4 copies its secret seed (`secretSeed` in that file) into `MINER_HOTKEY_URI`. The hotkey's mnemonic (`secretPhrase`) also works there; a wallet name, a file path or an SS58 address does not.

Note the hotkey's public SS58 address:

```bash
btcli wallet list
```

After chain registration, the Recall API admits your hotkey once its miner registry, refreshed by the subnet's master validator, includes it. Until then, the miner's registration attempts are refused with HTTP 403 and retried every 10 seconds, so you can start the miner before that.

## 3. Get the code

Install from the latest release of the public repository. Release tags are `v1.YYYYMMDD.NN`: the major version, the release date (UTC) and a two-digit number for that day, for example `v1.20261106.01`. Open <https://github.com/rec4ll-main/recall-miner/releases/latest>, note its tag, and clone at that tag:

```bash
TAG=<latest-release-tag>        # for example v1.20261106.01
git clone --branch "$TAG" https://github.com/rec4ll-main/recall-miner.git ~/recall-miner
cd ~/recall-miner
git describe --tags             # prints the release tag
```

Check that `~/recall-miner` is unused before cloning. Deploy a published tag rather than a moving branch. If the release page returns `404`, confirm the repository address and your access with the project before proceeding; do not substitute an unrelated repository. See [Operations](#10-operations) for upgrades.

## 4. Configure .env

The checkout's `.env` selects the Compose profile and supplies its settings. The block below is sufficient for a CPU miner; the [environment reference](docs/ENVIRONMENT.md) describes optional settings. `.env` contains your hotkey secret; how you protect the server is up to you.

### Required values (CPU)

Write the complete mainnet block:

```bash
(set -C; cat > .env <<'EOF'
# Compose settings for every docker compose command run in this directory
COMPOSE_PROJECT_NAME=recall-miner
COMPOSE_FILE=docker-compose.yml:deploy/compose/cpu.yml

# Keeps the startup secret checks on. This is not a chain selector.
ENVIRONMENT=production

# Recall API, mainnet (finney, netuid 31)
RECALL_API_URL=https://rec4ll.com/api
API_SIGNING_HOTKEY_SS58=5ECDAred992gLh55nJ7PxQDtihHXy5qZZXuCkqLVJdmDsrYw

# Secrets, filled in by the next commands
MINER_HOTKEY_URI=<miner-hotkey-secret>
MINER_INTERNAL_API_KEY=<random-hex>

# Public endpoint (native TLS)
MINER_HOST_PORT=6001
MINER_API_URL=https://<public-host>:6001
TLS_ENABLED=true
SIDECAR_MINER_API_URL=https://miner:8001
MINER_TLS_CA_FILE=/var/lib/recall-miner/certs/miner-cert.pem

# What this miner serves
MINER_EMBEDDING_MODELS=bge-small-en-v1.5
GENERATION_ENABLED=false
EOF
)
```

If `.env` already exists, the command refuses to overwrite it. For an existing miner, use [the update procedure](#update-to-a-new-release).

Fill in the hotkey secret (`secretSeed` from the hotkey file), a random internal key, and your public host. Replace `<wallet>` and `<hotkey>` with your wallet and hotkey names, and `miner.example.com` with your DNS name or public IP:

```bash
sed -i "s|^MINER_HOTKEY_URI=.*|MINER_HOTKEY_URI=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["secretSeed"])' ~/.bittensor/wallets/<wallet>/hotkeys/<hotkey>)|" .env
sed -i "s|^MINER_INTERNAL_API_KEY=.*|MINER_INTERNAL_API_KEY=$(openssl rand -hex 32)|" .env
sed -i "s|<public-host>|miner.example.com|" .env
grep -n '<' .env || echo "no placeholders left"
docker compose config --quiet && echo "config OK"
```

Expected: `no placeholders left` and `config OK`.

If this host already runs another miner, give this one its own `COMPOSE_PROJECT_NAME` and port; see [Several miners on one host](#several-miners-on-one-host).

The key settings are:

- `COMPOSE_PROJECT_NAME`: the stable name for this miner's containers and volumes.
- `COMPOSE_FILE`: the root file plus one CPU or GPU override.
- `API_SIGNING_HOTKEY_SS58`: the Recall API's public signing identity; it is separate from your miner hotkey and the internal service key.
- `MINER_API_URL`: the exact public HTTPS endpoint the API will connect to.
- `GENERATION_ENABLED=false`: keeps generation off until you configure and verify Chutes.

Keep `ENVIRONMENT=production` and the TLS settings as shown. The [environment reference](docs/ENVIRONMENT.md) explains how each setting reaches its service.

### `MINER_EMBEDDING_MODELS`

The miner sends this comma-separated list of catalog IDs with every registration and heartbeat. The Recall API routes a database, and embedding requests for its model, only to miners that declare that model. An unspecified database model means `bge-small-en-v1.5`.

Keep `bge-small-en-v1.5`. Both profiles serve it: the CPU inference service and vLLM load `BAAI/bge-small-en-v1.5` by default. The catalog also has `bge-m3`, `qwen3-embedding-0.6b`, `qwen3-embedding-4b` and `qwen3-embedding-8b`. Declare one of those only if a backend on this miner actually serves it; this guide's profiles do not. An empty value declares no model, and the miner then gets no database traffic for a catalog model.

### GPU profile

Switch the override and pin a vLLM image by digest. Use a `vllm/vllm-openai` tag you have tested with `BAAI/bge-small-en-v1.5`; replace `<tag>`:

```bash
sed -i 's|^COMPOSE_FILE=.*|COMPOSE_FILE=docker-compose.yml:deploy/compose/gpu.yml|' .env
docker pull vllm/vllm-openai:<tag>
echo "VLLM_IMAGE=$(docker image inspect --format '{{index .RepoDigests 0}}' vllm/vllm-openai:<tag>)" >> .env
grep '^VLLM_IMAGE=' .env           # VLLM_IMAGE=vllm/vllm-openai@sha256:...
docker compose config --quiet && echo "config OK"
```

The GPU override refuses to render without `VLLM_IMAGE`. On this profile vLLM serves embedding, and the CPU proxy serves only rerank.

### Optional values

See [Advanced settings](#11-advanced-settings) for tuning and [the environment reference](docs/ENVIRONMENT.md) for model, TLS and GPU settings.

### Do not set

Remove `VALIDATOR_HOTKEY_SS58` and `APPLICATION_PROBER_HOTKEY_SS58` from older configurations; the miner now uses `API_SIGNING_HOTKEY_SS58`. A removed name causes a startup error if it reaches the miner process.

`GEN_OPENAI_URL` is unsupported, and Chutes credentials must use a file rather than `CHUTES_API_KEY`. The proxy's `CAPABILITIES` and `MODELS` are selected by the Compose files, not by adding those names to `.env`.

## 5. TLS and the public endpoint

### Native TLS with Compose

The miner generates a self-signed certificate in the `miner-certs` volume. Its names include the public host and `miner`; it does not name your hotkey. The Recall API verifies the SHA-256 public-key pin sent in the signed registration, so a CA-issued certificate is unnecessary.

Sidecars trust the certificate at `MINER_TLS_CA_FILE` and load it when they start. [Step 7](#7-start) starts the miner first so that file exists. Keep the certificate volume through upgrades.

When a certificate changes, restart the sidecars after the miner has written the new certificate. This also applies when upgrading an older certificate that named the hotkey: the miner reissues it with the same key, preserving the pin, but the sidecars still need to reload it.

```bash
docker compose restart miner
# Wait for the certificate as in step 7.3, then:
docker compose restart miner-db-service cpu-model-proxy
# Also restart model-proxy (GPU) and chutes-genproxy (generation) when present.
```

### Firewall

Allow inbound TCP on the public port (`6001` by default) from anywhere, in your provider's firewall. Then check it from a machine outside your network after step 7:

```bash
curl -sk https://<public-host>:6001/health; echo
# 404 page not found
```

An unsigned request from a public address returns this plain `404` on every path. It confirms that something answers over TLS; [step 8.2](#82-from-outside) verifies that it is your miner's key. Local and private-network health checks still return the miner's health response. Run uptime monitoring from the server or your private network.

## 6. Optional: generation with Chutes

Generation runs only through the Chutes sidecar (Compose profile `chutes`). It adds two services: `chutes-generation`, which calls Chutes, and `chutes-genproxy`, which advertises `generate`. Skip this section if you are not adding generation.

### 6.1 Account, key and model

1. Create a Chutes account and an API key. Use a separate key for each miner.
2. Use the recommended model `Qwen/Qwen3.8-27B-TEE`. To use another one, see [Advanced settings](#11-advanced-settings); model IDs must be an exact `org/name`, and aliases and pools are refused.
3. After storing the key in step 6.2, send one small request to check it and the model's current capacity. The provider may bill this request:

   ```bash
   sed 's/^/Authorization: Bearer /' ~/chutes-key/api-key |
     curl -sS -o /dev/null -w '%{http_code}\n' -H @- -H 'Content-Type: application/json' \
       https://llm.chutes.ai/v1/chat/completions \
       -d '{"model":"Qwen/Qwen3.8-27B-TEE","messages":[{"role":"user","content":"Say OK"}],"max_tokens":16}'
   ```

   `200` confirms this request succeeded; it does not reserve future capacity. `429` indicates provider rate limiting or capacity pressure, `401` an authentication failure and `402` a billing failure. The sidecar does not retry an HTTP `429`.

### 6.2 Key file

The sidecar reads the key from the file `api-key` in the directory `CHUTES_KEY_DIR`. Put the key there (paste it into the editor and save):

```bash
mkdir -p ~/chutes-key
nano ~/chutes-key/api-key
chmod 644 ~/chutes-key/api-key
```

The sidecar runs as UID 1001, so the file must be readable by it, and it refuses a file that group or others can write. The sidecar keeps its request ledger in the Docker volume `chutes-state`, which Compose creates automatically.

### 6.3 Add the generation settings

```bash
cat >> .env <<'EOF'

# Generation through Chutes
COMPOSE_PROFILES=chutes
CHUTES_MODEL=Qwen/Qwen3.8-27B-TEE
EOF
echo "CHUTES_KEY_DIR=$HOME/chutes-key" >> .env
```

- `COMPOSE_PROFILES=chutes` adds the two generation services to every `docker compose` command run here. It is the same as passing `--profile chutes`.
- Compose passes this one `CHUTES_MODEL` to the miner, the sidecar and the generation proxy, so all three agree. Keep the line: without it the sidecar refuses generation ([why](docs/ENVIRONMENT.md#generation-model-chutes_model)).
- `GENERATION_ENABLED` stays `false` until step 7.4.

## 7. Start

Run these commands from the checkout containing `.env`. Compose reads the selected CPU or GPU override and the optional Chutes profile from that file.

### 7.1 Build the images

```bash
GIT_COMMIT=$(git rev-parse --short=7 HEAD) docker compose build
```

`GIT_COMMIT` puts the commit into the miner binary, which reports it as `miner_version` `recall-miner/<commit>`; without it you get `recall-miner/dev`. The first build downloads several GB.

### 7.2 GPU profile only: check the GPU and pull vLLM

```bash
docker run --rm --runtime=nvidia --gpus all ubuntu nvidia-smi
docker compose pull vllm
```

`nvidia-smi` must list your GPU. If it does not, fix the driver or the NVIDIA Container Toolkit before you continue.

### 7.3 Start the miner first

On the first start, the miner creates its certificate before it serves, and the sidecars need that file when they start. Start the miner alone and wait for the certificate:

```bash
docker compose up -d miner
for i in $(seq 30); do docker compose exec -T miner test -s /var/lib/recall-miner/certs/miner-cert.pem && break; sleep 2; done
docker compose exec -T miner test -s /var/lib/recall-miner/certs/miner-cert.pem && echo "certificate ready"
```

Expected: `certificate ready`. If it does not appear, run `docker compose logs miner`; a configuration error stops the miner before it writes the certificate (see [Troubleshooting](#12-troubleshooting)).

### 7.4 Start everything else

```bash
docker compose up -d
docker compose ps
```

Every service should be `running`:

- CPU: `miner`, `qdrant`, `miner-db-service`, `miner-inference-service`, `cpu-model-proxy`.
- GPU adds `vllm` and `model-proxy`.
- Generation adds `chutes-generation` and `chutes-genproxy`.

On the GPU profile, vLLM downloads and loads its model first. `embedding` is advertised once vLLM answers its health check.

The ordering matters only while the certificate does not exist yet. After that, restarts and reboots can start everything at once.

**Generation only:** check that the sidecar is ready, then enable generation.

```bash
docker compose exec -T chutes-generation python -c "
import urllib.request, urllib.error
try:
    r = urllib.request.urlopen('http://127.0.0.1:19201/health/generation', timeout=5); print(r.status, r.read().decode())
except urllib.error.HTTPError as e:
    print(e.code, e.read().decode())"
```

Expected:

```
200 {"status":"ok","reason":"","connector":"chutes","model":"Qwen/Qwen3.8-27B-TEE","hosted_deadline_seconds":58.0}
```

If you see `Connection refused`, the sidecar is still starting: wait 10 seconds and run it again. A `503` names the reason; see [Troubleshooting](#12-troubleshooting). When it is `200`:

```bash
sed -i 's|^GENERATION_ENABLED=.*|GENERATION_ENABLED=true|' .env
docker compose up -d
```

This recreates only the miner. The sidecars re-register with it within seconds.

## 8. Verify

### 8.1 On the server

The miner answers locally:

```bash
curl -sk https://127.0.0.1:6001/health; echo
```

```
{"service":"recall-miner","status":"ok"}
```

Check that the sidecars are registered and healthy. The internal key goes to `curl` on stdin, so it never appears in a command line. The script also works while containers are still starting:

```bash
sed -n 's/^MINER_INTERNAL_API_KEY=/X-Internal-Api-Key: /p' .env |
  curl -fsSk -H @- https://127.0.0.1:6001/internal/services |
  python3 -c '
import json, sys
raw = sys.stdin.read()
if not raw.strip():
    sys.exit("no answer from the miner yet: check docker compose ps and docker compose logs miner")
services = json.loads(raw).get("services") or {}
if not services:
    print("no sidecars registered yet")
for name, svc in sorted(services.items()):
    caps = ",".join(svc.get("capabilities") or [])
    status = (svc.get("health") or {}).get("status", "waiting for first heartbeat")
    print(name, caps, status)'
```

Expected on CPU (GPU shows `cpu rerank ok` and `gpu embedding ok`; generation adds `chutes generate ok`):

```
cpu embedding,rerank ok
db db ok
```

`waiting for first heartbeat` is normal for a few seconds after a sidecar registers. A model proxy marked `degraded` cannot reach its backend, and the miner does not advertise its capability. A sidecar that stops sending heartbeats drops out of this list after 60 seconds. The `db` sidecar always reports `ok`; it does not check Qdrant itself.

Check the miner log:

```bash
docker compose logs miner | grep -E 'miner hotkey loaded|registered sidecar|Recall API'
```

You should see:

- `miner hotkey loaded hotkey=<SS58>`, matching your registered hotkey;
- one `registered sidecar` line per sidecar, with its capabilities;
- `registered with Recall API`, followed by silence. Heartbeats log only when they fail, as `Recall API heartbeat failed`.

`Recall API registration failed` lines mean the API refused the registration. The error text says why; see [Troubleshooting](#12-troubleshooting).

### 8.2 From outside

Run this from another machine. It checks that the endpoint presents your miner's own key:

```bash
# On the miner server: the certificate is public, the key never leaves the volume
docker compose cp miner:/var/lib/recall-miner/certs/miner-cert.pem ~/miner-cert.pem
openssl x509 -in ~/miner-cert.pem -noout -subject -ext subjectAltName
```

The subject is empty (the certificate names no hotkey), and the names include your public host and `miner`. Copy `miner-cert.pem` to the outside machine, then:

```bash
curl --cacert miner-cert.pem https://<public-host>:6001/health; echo
spki() { openssl x509 -pubkey -noout | openssl pkey -pubin -outform DER | openssl dgst -sha256 -binary | openssl base64 -A; }
[ "$(spki < miner-cert.pem)" = "$(openssl s_client -connect <public-host>:6001 -servername <public-host> </dev/null 2>/dev/null | spki)" ] && echo "pin OK"
```

Expected: `404 page not found` (the outside check is not signed by the Recall API) and `pin OK`.

### 8.3 On the public leaderboard

Look up your hotkey's SS58 address on the Recall public leaderboard at <https://rec4ll.com>. A healthy miner shows:

| Field | Healthy value |
|---|---|
| Status | `online`: recent heartbeats |
| Capabilities | `db`, `embedding`, `rerank`, plus `generate` with generation |
| TLS | a TLS pin is held for your miner |
| Miner version | `recall-miner/<commit>`: the commit you built |
| Probes | probe counts and pass rates per tier, filling in over the first hour |

If the hotkey is not listed, the API's registry does not know it yet (step 2).

### 8.4 How long until probes arrive

1. **Chain registration to API admission:** once the subnet's master validator refreshes the API's miner registry (step 2). The miner retries its registration every 10 seconds, and sends a heartbeat every 30 seconds once registered.
2. **Admission to `online`:** a successful registration counts as a heartbeat.
3. **`online` to the first probes:** the prober probes online miners on a schedule, so the first probe for each tier arrives some minutes later. Vector DB and embedding probes need `bge-small-en-v1.5` in `MINER_EMBEDDING_MODELS`. Generation probes need the `generate` capability.
4. **Counts:** per-tier counts fill over the first hour. Scoring uses evidence collected over several hours (section 9).

## 9. How scoring works

The Recall API collects probe results and records verified customer requests. Validators use that evidence to determine eligibility and allocate rewards for each service tier.

Readiness measures whether a miner can serve a tier correctly. Delivered service measures eligible customer work. The allocation depends on the active validator policy, evidence window, other eligible miners and weight caps. Registration, hardware or a single successful request does not guarantee rewards.

For the current rules and defaults, see the [Recall documentation](https://rec4ll.com/docs) and [validator allocation settings](https://github.com/rec4ll-main/recall-validator/blob/main/docs/MASTER.md). Use the deployed validator release when checking a specific epoch.

## 10. Operations

### Logs and restarts

```bash
docker compose ps
docker compose logs -f --tail 100 miner        # or any service name
docker compose restart miner-db-service        # restart one service
docker compose up -d                           # apply .env changes; recreates only what changed
```

Containers use `restart: unless-stopped`. They come back after a reboot when Docker starts at boot (`sudo systemctl enable docker`), unless you stopped them.

### Update to a new release

Check <https://github.com/rec4ll-main/recall-miner/releases/latest> for a newer tag, then:

```bash
cd ~/recall-miner
NEW_TAG=<new-release-tag>                      # for example v1.20261106.01
git fetch --tags origin
git diff HEAD "$NEW_TAG" -- .env.example       # review new or changed settings
git checkout --detach "$NEW_TAG"
GIT_COMMIT=$(git rev-parse --short=7 HEAD) docker compose build
docker compose up -d
docker compose logs --tail 50 miner
```

Read the release notes before upgrading, including any certificate or storage migration steps. Keep `.env`, `COMPOSE_PROJECT_NAME` and all persistent volumes. Repeat [the verification checks](#8-verify); after a successful heartbeat, the public record should show the new commit.

For rollback, check the release's storage compatibility before rebuilding the previous tag. A code rollback does not undo data or ledger changes. Never restore an older generation ledger to make a rollback start.

### Volumes and backups

| What | Where | Holds | If lost |
|---|---|---|---|
| Vector databases | volume `<project>_qdrant-data` | Databases the API placed on this miner | Their data is gone and their probes fail. Back it up. |
| TLS certificate and key | volume `<project>_miner-certs` | Your registered pin | A new certificate and pin are registered at the next start; restart the stack (section 5) |
| Miner metrics | volume `<project>_miner-data` | Local heartbeat metrics | Nothing to restore |
| Model caches | volumes `<project>_inference-model-cache`, `<project>_vllm-cache` | Downloaded models | Downloaded again |
| Generation ledger | volume `<project>_chutes-state` | Record of every Chutes request | Keep it on durable local storage. Never delete, edit or share it, and never restore an older copy over it ([why](docs/GENERATION_CHUTES.md#failure-behaviour-by-design)). |
| Chutes key | `CHUTES_KEY_DIR` on the host | The API key | Create a new key at Chutes |
| Configuration | `.env` | Secrets and settings | Keep a private backup |

Back up a volume with the stack stopped. This example backs up the vector databases:

```bash
docker compose stop
mkdir -p ~/backups
docker run --rm -v recall-miner_qdrant-data:/data:ro -v ~/backups:/backup debian:trixie-slim tar -C /data -czf /backup/qdrant-data.tgz .
docker compose start
```

### Several miners on one host

Give each miner its own checkout directory, and therefore its own `.env`, and change everything that identifies it:

| Setting | Miner 1 | Miner 2 |
|---|---|---|
| Checkout directory | `~/recall-miner` | `~/recall-miner-2` |
| `COMPOSE_PROJECT_NAME` | `recall-miner` | `recall-miner-2` |
| `MINER_HOST_PORT` and the port in `MINER_API_URL` | `6001` | `6002` |
| Coldkey | its own | its own |
| Hotkey (`MINER_HOTKEY_URI`) | its own | its own |
| `MINER_INTERNAL_API_KEY` | its own | its own |
| Chutes key, `CHUTES_KEY_DIR` | its own | its own |

Clone the second checkout at the same release tag with `git clone --branch "$TAG" https://github.com/rec4ll-main/recall-miner.git ~/recall-miner-2`. Then repeat steps 4 to 8 there, and open its port. Never share volumes, ledgers, keys or hotkeys between miners. Each miner's hotkey must be registered from a different coldkey ([one hotkey per coldkey](#2-wallet-and-chain-registration)).

### Move to a new hotkey

Follow [docs/FRESH_START.md](docs/FRESH_START.md). It sets up an independent stack and keeps the old one's data offline.

### Stop safely

```bash
docker compose stop      # stops everything; containers and volumes stay
docker compose down      # removes the containers; volumes stay
```

Never run `docker compose down -v` or `docker volume prune` as a restart or troubleshooting step: they delete the vector databases and the TLS key. A stopped miner sends no heartbeats.

## 11. Advanced settings

The steps above need no tuning. [docs/ENVIRONMENT.md, Advanced settings](docs/ENVIRONMENT.md#advanced-settings) lists the settings a miner may change, with defaults and the service each one reaches:

- `CHUTES_MODEL`: the Chutes model used for generation. Set it in `.env` to another exact model ID and run `docker compose up -d`; Compose passes it to the miner, the sidecar and the generation proxy together.
- `CHUTES_MAX_CONCURRENT` (default `4`, `1`-`16`): how many generation requests your miner sends to Chutes at once; match it to your Chutes plan. Set it in `.env` and run `docker compose up -d`.
- `GENERATION_BACKEND_TIMEOUT_SECONDS` (default `58`, which fits the API's 60-second scoring window) and `GENERATE_MAX_INFLIGHT` (default `8`): the miner's generation deadline and how many generation requests it accepts at once. Raise `GENERATE_MAX_INFLIGHT` if you set `CHUTES_MAX_CONCURRENT` above `8`.
- Batch sizes, vLLM memory and context length, heartbeat intervals, and the miner's overload controls (`RATE_LIMIT_*`, `*_MAX_INFLIGHT`).

## 12. Troubleshooting

Start with `docker compose ps` and `docker compose logs --tail 100 miner`. Configuration errors appear after `initialize api:` and usually leave the miner restarting. Keep secrets out of any logs you share.

### Configuration and startup

| Symptom | What to check |
|---|---|
| A removed `VALIDATOR_HOTKEY_SS58` or `APPLICATION_PROBER_HOTKEY_SS58` setting | Delete it from overrides and service definitions; use `API_SIGNING_HOTKEY_SS58` |
| Empty or invalid API signing identity | Use the public Recall API identity from step 4 |
| Missing or placeholder `MINER_INTERNAL_API_KEY` | Generate a random key with the rotation command below, then recreate the miner and sidecars |
| Empty, placeholder or invalid `HOTKEY_URI` | Check that `.env` contains the registered hotkey's seed; copy the correct hotkey file again if needed and follow step 4 |
| GPU profile requires `VLLM_IMAGE` | Pin a tested image as described in step 4 |
| GPU services start on a CPU host | Check `COMPOSE_FILE` and run Compose from the checkout containing `.env` |
| vLLM cannot see the GPU | Check the NVIDIA driver and container toolkit with the command in step 7.2 |
| Certificate-volume permission error | Check this project's volume ownership; the miner runs as UID 1000 |

To replace only this stack's internal key, without needing the temporary hotkey file again:

```bash
python3 - <<'PYTHON'
import os, secrets
from pathlib import Path
path = Path('.env')
lines = path.read_text().splitlines()
lines = [line for line in lines if not line.startswith('MINER_INTERNAL_API_KEY=')]
lines.append('MINER_INTERNAL_API_KEY=' + secrets.token_hex(32))
fd = os.open('.env.tmp', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as f:
    f.write('\n'.join(lines) + '\n')
os.replace('.env.tmp', path)
PYTHON
docker compose up -d
```

Recreating services briefly interrupts requests. Repeat the sidecar and registration checks afterward. If `.env.tmp` already exists, inspect why the earlier write was interrupted before retrying.

For an incorrectly owned miner volume, repair only this Compose project's mounts:

```bash
docker compose run --rm --no-deps --user root --entrypoint chown miner \
  -R miner:miner /var/lib/recall-miner
docker compose up -d
```

### Registration and connectivity

| Symptom | What to check |
|---|---|
| Registration `403` | Match `miner hotkey loaded` in the log to the registered subnet-31 hotkey. Allow time for the API's registry to refresh. |
| Registration `400` about the endpoint | Use a public `https://host:port` URL in `MINER_API_URL`. |
| Registration `400` about TLS or `tls_spki_sha256` | Check inbound TCP, NAT, host and port. Compare the certificate pins as in step 8.2; a TLS-terminating proxy presents the wrong key. |
| Unsigned public `/health` returns `404` | Expected. Use a local health check and the external pin check. |
| Sidecar certificate errors or `invalid MINER_TLS_CA_FILE` | Start the miner and wait for the certificate, then restart the affected sidecars. They load the certificate once at startup. |
| Miner remains offline | Check containers, registration errors and whether the hotkey is still registered on-chain. |
| Online but missing probes | Check healthy advertised capabilities and `MINER_EMBEDDING_MODELS=bge-small-en-v1.5`; probe history takes time to accumulate. |

### Generation

Inspect `docker compose logs --tail 100 chutes-generation` and `/health/generation` from step 7.4.

| Symptom | What to check |
|---|---|
| Provider `429` | Provider rate limits or model capacity; reduce concurrency or use an available model |
| Provider `401` or `402` | Credential or billing problem; restart the sidecar after changing its key file |
| `completed_unaccepted` | Provider response failed validation, such as a mismatched model identity |
| Credential file unavailable | `api-key` must exist and be readable by UID/GID 1001; use step 6.2 permissions |
| Missing bind source path | Check that `CHUTES_KEY_DIR` exists and matches `.env` |
| Ledger not writable | Correct permissions on the existing ledger storage; do not switch to an empty volume to bypass the error |
| Ledger missing or identity mismatch | Check the original mount and any `CHUTES_LEDGER_ID` pin; preserve the existing ledger |
| `unresolved hosted attempt held` | Recent provider attempts have unknown outcomes; they stop counting against admission after five minutes |
| Invalid model, timeout or concurrency | Correct the named setting, recreate affected services and repeat the health check |

See [the Chutes reference](docs/GENERATION_CHUTES.md) for full error meanings, retry behavior and ledger inspection. Never delete or restore an older ledger to clear an error.

## 13. FAQ

**Can I serve a different embedding model?** Not by renaming. Each database is tied to one embedding model, and probes use `bge-small-en-v1.5`. Keep the default.

**Can I use a different generation model?** Yes: set `CHUTES_MODEL` ([Advanced settings](#11-advanced-settings)).

**Do I have to run all tiers?** No. The miner advertises what its healthy sidecars serve, and the API probes what is advertised. The CPU and GPU profiles here serve T1 to T3; generation adds T4.
