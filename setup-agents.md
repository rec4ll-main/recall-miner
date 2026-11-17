# Miner deployment checklist

Use this checklist for a repeatable installation or an operator handover. The commands and expected results live in [SETUP.md](SETUP.md); use the copy from the release you deploy.

## Before you start

Record these non-secret deployment details:

| Input | What to confirm |
|---|---|
| Release | Published tag and commit; no local changes to tracked files |
| Host and port | Public IP or DNS name and an unused inbound TCP port, usually `6001` |
| Hotkey | Public SS58 address registered on finney subnet 31 |
| Profile | CPU or GPU; GPU needs a tested vLLM image pinned by digest |
| Generation | Whether to enable Chutes, with its own credential and provider capacity |
| Isolation | An unused checkout directory, Compose project name and data volumes |

Check available CPU, memory and disk against [the sizing guidance](SETUP.md#hardware). Smaller hosts need workload measurements before accepting production traffic. Do not replace another stack's configuration or reuse its hotkey, port or volumes.

## Install

1. [Prepare the wallet](SETUP.md#2-wallet-and-chain-registration). Register the hotkey from a coldkey that has no other miner hotkey on subnet 31 (only the newest hotkey of a coldkey earns); the guide assumes it is already in `~/.bittensor/wallets` on the server.
2. [Get the release](SETUP.md#3-get-the-code). Use the public release tag and its accompanying documentation.
3. [Configure the miner](SETUP.md#4-configure-env). Generate a unique internal API key and select one Compose override. Keep production checks enabled.
4. [Configure native TLS](SETUP.md#5-tls-and-the-public-endpoint). Publish only the miner port. A TLS-terminating proxy changes the key the Recall API expects.
5. If needed, [configure Chutes](SETUP.md#6-optional-generation-with-chutes). Store the key outside the checkout and keep generation disabled until its health check passes. A provider test may incur a charge.
6. [Build and start](SETUP.md#7-start). Run the volume-permission check, start the miner, wait for its certificate, then start the remaining services.
7. [Verify the deployment](SETUP.md#8-verify) and record the results below.

For automation, validate configuration with `docker compose config --quiet`. The full rendered configuration contains secrets. Keep secrets out of command-line arguments, logs and deployment reports. Use the file-based commands in the install guide.

## Acceptance checks

- Every service selected by the profile is running.
- Every expected sidecar is registered and healthy.
- The miner log reports `registered with Recall API`.
- The endpoint presents the expected TLS public-key pin from another machine. An unsigned public `/health` request returns `404`; this is expected.
- The public miner record shows the intended hotkey, `online` status, TLS pin, release commit and capabilities.
- With generation, `/health/generation` is healthy before enabling `GENERATION_ENABLED`.

These checks establish installation and connectivity. Record any missing or failed checks and use [troubleshooting](SETUP.md#12-troubleshooting) before calling the deployment ready. Probe history and reward eligibility take longer to establish.

## Handover and updates

Record the release, host, port, public hotkey, Compose project, enabled capabilities, verification results and unresolved issues. Record where protected backups are kept, not their contents.

Keep `.env` and the persistent volumes through upgrades. With Chutes, keep the generation ledger too. Follow [the update procedure](SETUP.md#update-to-a-new-release), then repeat the acceptance checks.

Use `docker compose stop` or `docker compose restart` for routine maintenance. `docker compose down -v`, volume removal and volume pruning delete persistent state; they are not recovery steps. A hotkey change needs the [fresh instance procedure](docs/FRESH_START.md).
