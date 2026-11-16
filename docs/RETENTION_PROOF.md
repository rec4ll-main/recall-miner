# Retention proof endpoint

The Recall API uses a retention proof to check that a miner still holds a particular vector and its metadata without returning the stored content. This page defines the miner endpoint and its encoding contract.

## Request and response

Send an Epistula-signed `POST /db/{db_id}/retention-proof` from the configured API signing identity. The JSON body has exactly two fields:

```json
{"embed_id":"document-1","nonce":"<64 hex characters>"}
```

The nonce is 32 bytes encoded as 64 hexadecimal characters. The response contains `db_id`, `embed_id` and a hexadecimal HMAC-SHA256 `proof`. It contains no vector, text or metadata.

The route uses the miner's signature, path/body envelope, timestamp and replay checks, response signing, per-signer limit and query concurrency limit. The miner requires a pinned database service and checks that assignment again before responding.

The API establishes the caller's database ownership before signing the request. The sidecar's internal API key authenticates service calls; it is not an end-user credential. The Qdrant reader checks the logical database's current generation, physical point ID, tenant ID and embedding ID. It does not search another database or fall back to a legacy point ID.

## Proof encoding

Use the nonce bytes as the HMAC key. The message contains these four fields in order, each preceded by its byte length as a four-byte big-endian integer:

1. Embedding ID in UTF-8.
2. Stored vector components as little-endian float32 values.
3. The stored metadata's `text` value in UTF-8, or an empty value when absent.
4. Remaining metadata as JSON, with Go `encoding/json` key ordering.

Metadata goes through a float64 JSON round trip to match the API verifier. The stored, cosine-normalized vector is authoritative. Challenge vectors must use the verifier's pre-normalized float32 representation.

## Bounds and errors

| Limit | Value |
|---|---|
| Request body | 1 KiB |
| Identifiers | 1–128 ASCII letters, digits, underscores or hyphens |
| Sidecar response | 1 KiB, checked before JSON decoding |
| Backend record response | 2 MiB |
| Vector length | At most 65,536 components |
| Operation context | 5 seconds |

Duplicate or unknown JSON fields, trailing values, malformed nonces and invalid stored shapes are rejected. Oversized records are rejected rather than partially proved.

| Status | Meaning |
|---|---|
| `404` | Record not found |
| `502` | Backend error or invalid stored data |
| `501` | Backend does not support this operation; the supported implementation is Qdrant |

## Nonce handling

The challenge issuer is responsible for nonce freshness. The endpoint is stateless: a newly signed request can recompute the proof for the same nonce. An identical signed HTTP request is rejected by the replay cache.

This contract has no epoch field, nonce-consumption ledger or payout operation. Providing the endpoint does not itself enable storage rewards; reward policy belongs to the validator and API configuration.
