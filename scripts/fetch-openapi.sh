#!/usr/bin/env bash
# Re-vendor api/openapi.yaml from plane-shift/aether-api at the SHA in OPENAPI_REF.
# Usage: scripts/fetch-openapi.sh [<full-sha>]   (writes OPENAPI_REF when a sha is given)
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "${1:-}" != "" ]; then
  [ ${#1} -eq 40 ] || { echo "need a full 40-char sha" >&2; exit 1; }
  printf '%s\n' "$1" > OPENAPI_REF
fi
REF=$(tr -d '[:space:]' < OPENAPI_REF)
[ ${#REF} -eq 40 ] || { echo "OPENAPI_REF must hold a full sha" >&2; exit 1; }
mkdir -p api
# gh api works for the private repo (token) and the public one alike.
gh api -H 'Accept: application/vnd.github.raw' "repos/plane-shift/aether-api/contents/api/openapi.yaml?ref=$REF" > api/openapi.yaml.new
python3 -c "import yaml,sys; yaml.safe_load(open('api/openapi.yaml.new'))" || { rm -f api/openapi.yaml.new; echo "fetched spec does not parse" >&2; exit 1; }
mv api/openapi.yaml.new api/openapi.yaml
echo "vendored api/openapi.yaml at $REF"
