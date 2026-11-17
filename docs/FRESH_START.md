# Fresh miner setup with a new hotkey

Use this procedure to create an independent miner identity while preserving the old instance for recovery. It does not migrate or delete the old data, and a new hotkey alone does not safely reset a database or transfer customer ownership.

## Before stopping the old instance

1. Record the old instance's release tag and commit, Compose project name, endpoint, public hotkey, and named volume names. Keep its private `.env` and wallet backup in your existing protected operator storage. Do not paste secrets into the deployment record.
2. Check whether the old miner owns customer databases or is serving traffic. Coordinate draining, backups and any API-side reassignment/migration with the Recall operator before retiring it. Old pinned database ownership will not move automatically to a new hotkey.
3. Prepare a separate checkout directory for the new instance, cloned at the latest release tag ([SETUP.md, step 3](../SETUP.md#3-get-the-code)). Do not edit the old instance in place. Choose a new, unused Compose project name, public endpoint/port, and registered hotkey. Use distinct hotkeys for distinct miner instances. Only one miner hotkey per coldkey earns incentive on subnet 31: the most recently registered one ([SETUP.md, step 2](../SETUP.md#2-wallet-and-chain-registration)). If you register the new hotkey under the same coldkey, the old hotkey stops earning from the next epoch, before the new one has passed its readiness checks.
4. Create and register the new hotkey with `btcli` on your wallet machine ([SETUP.md, step 2](../SETUP.md#2-wallet-and-chain-registration)). Confirm its public SS58 and its UID on finney subnet 31. The registration cost is paid from your coldkey. Do not send the seed to another operator or into chat.

## Isolate the new stack

In the new checkout, write `.env` as described in [SETUP.md, step 4](../SETUP.md#4-configure-env), and fill in the new hotkey and a fresh internal key with the commands there. The values that must differ from the old instance, or must be present:

```env
COMPOSE_PROJECT_NAME=recall-miner-new-identity
COMPOSE_FILE=docker-compose.yml:deploy/compose/cpu.yml
ENVIRONMENT=production
RECALL_API_URL=https://rec4ll.com/api
API_SIGNING_HOTKEY_SS58=5ECDAred992gLh55nJ7PxQDtihHXy5qZZXuCkqLVJdmDsrYw
MINER_HOTKEY_URI=<new-miner-hotkey-secret>
MINER_INTERNAL_API_KEY=<fresh-random-key>
MINER_HOST_PORT=6001
MINER_API_URL=https://<your-public-host>:6001
TLS_ENABLED=true
SIDECAR_MINER_API_URL=https://miner:8001
MINER_TLS_CA_FILE=/var/lib/recall-miner/certs/miner-cert.pem
MINER_EMBEDDING_MODELS=bge-small-en-v1.5
GENERATION_ENABLED=false
```

Use `deploy/compose/gpu.yml` and `VLLM_IMAGE` on a GPU host. With generation, give the new instance its own Chutes key and key directory. Its Compose project creates a separate `chutes-state` volume and the sidecar initializes the ledger on first start ([SETUP.md, step 6](../SETUP.md#6-optional-generation-with-chutes)); never reuse the old ledger. Do not set `VALIDATOR_HOTKEY_SS58` or `APPLICATION_PROBER_HOTKEY_SS58`: they were removed, and the miner refuses to start when either reaches its environment. The angle-bracket values are placeholders.

Use a project name that has never been used for another instance. The standard Compose file has no explicit shared volume names, so a new project creates separate `qdrant-data`, `miner-data`, `miner-certs`, `vllm-cache` and `inference-model-cache` volumes. Check any deployment overrides for `external: true`, fixed `name:` values or host bind mounts that could accidentally reconnect old data. A fresh project name is not sufficient if an override explicitly shares a volume.

Do not mount or copy the old `miner-certs` volume into the new identity; the new miner creates its own certificate and registers its signed SPKI pin. Do not attach old Qdrant or miner data to the new identity as an implicit migration. Fresh model caches are acceptable, but wait for downloads and actual capability probes before declaring readiness.

## Cut over without deleting old state

1. After traffic/data obligations are addressed, stop the old stack using its original project and checkout directory: `docker compose stop`. Stopping keeps its containers and named volumes. Do not run `down -v`, `docker volume prune`, remove volumes, or delete the old checkout directory.
2. In the new directory, validate the configuration with `docker compose config --quiet`. Confirm the project, endpoint, selected services and volume mappings. The full rendered configuration contains secrets; inspect it only in a private terminal.
3. Follow the certificate-first sequence in [SETUP.md, step 7](../SETUP.md#7-start): start the miner, wait for its certificate, then start the sidecars. Do not start sidecars against a missing CA file.
4. Verify the public endpoint presents the native certificate pin; confirm the new public hotkey is the signer for API registration, fresh heartbeats and signed responses. Check the hotkey's actual finney subnet-31 UID and registration independently.
5. Follow the [verification steps](../SETUP.md#8-verify) and confirm that the Recall API's signed probes pass for every advertised capability. Confirm expected API routing before moving customer traffic. A local `GET /health` response alone is insufficient.
6. Keep the old instance stopped and its volumes offline until the agreed retention/recovery decision. Do not run both stacks on the same public port, identity, or shared data volumes.

## Recovery

If the new stack fails acceptance, stop it first. Preserve its logs, configuration and volumes for diagnosis without exposing secrets. Subject to the coordinated data/routing plan, restart the old stack from its original directory and unchanged project name. Verify its endpoint, certificate pin, registration and sidecars again. If any data migrated or writes reached the new instance, reconcile those before rolling traffic back; restarting old containers is not by itself a data rollback.

Keep data migration and permanent retirement separate from this identity change.
