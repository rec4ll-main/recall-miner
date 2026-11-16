#!/usr/bin/env bash
# Clean-image import smoke: same layout as Dockerfile.inference-service.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
REPO="$(cd "$ROOT/../../.." && pwd)"
# Copy exactly the modules the image ships, read from its COPY lines, so this
# smoke cannot drift from Dockerfile.inference-service again.
files="$(grep -E '^COPY ' "$REPO/Dockerfile.inference-service" | grep -oE 'internal/services/inference/[A-Za-z0-9_.-]+' | sort -u)"
[ -n "$files" ] || { echo "no inference COPY lines in Dockerfile.inference-service" >&2; exit 1; }
for f in $files; do
  cp "$REPO/$f" "$TMP/$(basename "$f")"
done
cd "$TMP"
# Avoid importing heavy ML stacks: stub transformers/sentence_transformers.
python3 - <<'PY'
import sys, types
sys.modules['sentence_transformers'] = types.SimpleNamespace(CrossEncoder=object, SentenceTransformer=object)
sys.modules['transformers'] = types.SimpleNamespace(AutoModelForCausalLM=object, AutoTokenizer=object)
import concurrency
import service
assert hasattr(service, 'Handler')
assert hasattr(concurrency, 'GenerationLimiter')
print('SMOKE_IMPORT_OK')
PY
