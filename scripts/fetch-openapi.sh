#!/usr/bin/env bash
# Re-vendor api/openapi.yaml from plane-shift/aether-api.
#
# Usage: scripts/fetch-openapi.sh [<full-sha>]
#   With a sha: vendor that revision, then record it in OPENAPI_REF.
#   Without:    re-vendor the revision OPENAPI_REF already names.
#
# ORDERING IS THE POINT. OPENAPI_REF used to be written BEFORE the fetch, so a
# failed or unparseable download left the pin claiming a revision the checked-in
# spec was not taken from - and the pin is what every later "what is this
# generated from?" question is answered with. The pin is now written LAST, only
# after a validated spec is in place, so the two can never disagree: either both
# move or neither does.
set -euo pipefail
cd "$(dirname "$0")/.."

candidate="${1:-$(tr -d '[:space:]' < OPENAPI_REF)}"
# A short sha, a branch name or a tag would all "work" against the API and then
# sit in OPENAPI_REF as something that can move under us.
[[ "$candidate" =~ ^[0-9a-f]{40}$ ]] || {
  echo "need a full 40-character lowercase-hex commit sha, got: ${candidate}" >&2
  exit 1
}

mkdir -p api
tmp="api/openapi.yaml.new"
# Leave no half-written file behind on any exit path, including a failed fetch.
trap 'rm -f "$tmp"' EXIT

# gh api works for the private repo (token) and the public one alike.
gh api -H 'Accept: application/vnd.github.raw' \
  "repos/plane-shift/aether-api/contents/api/openapi.yaml?ref=${candidate}" > "$tmp"

python3 -c "import yaml,sys; yaml.safe_load(open('$tmp'))" || {
  echo "fetched spec does not parse as YAML; leaving api/openapi.yaml and OPENAPI_REF untouched" >&2
  exit 1
}

mv "$tmp" api/openapi.yaml
# Only now, with the spec actually replaced, does the pin move.
printf '%s\n' "$candidate" > OPENAPI_REF
echo "vendored api/openapi.yaml at $candidate"
