#!/usr/bin/env sh
# Builds the Caddy binary RASA bundles.
#
# Stock Caddy ships NEITHER module the generated config uses, and a stock
# binary fails at start with an unrecognised-directive error -- after RASA has
# already reported success. Versions are pinned deliberately: an unpinned build
# means the proxy changes underneath users between releases (SPEC.md 17).
set -eu

CADDY_VERSION="${CADDY_VERSION:-v2.10.0}"
DYNU_VERSION="${DYNU_VERSION:-v1.0.0}"
RATELIMIT_VERSION="${RATELIMIT_VERSION:-v0.1.0}"

OUT="${OUT:-./dist/caddy}"
mkdir -p "$(dirname "$OUT")"

command -v xcaddy >/dev/null 2>&1 || {
  echo "xcaddy is required: go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest" >&2
  exit 1
}

echo "building caddy ${CADDY_VERSION}"
xcaddy build "${CADDY_VERSION}" \
  --with "github.com/caddy-dns/dynu@${DYNU_VERSION}" \
  --with "github.com/mholt/caddy-ratelimit@${RATELIMIT_VERSION}" \
  --output "$OUT"

# Prove the modules are present. Catching this here is the whole point: the
# alternative is discovering it when a user's proxy refuses to start.
echo "verifying required modules"
"$OUT" list-modules | grep -q "dns.providers.dynu" || {
  echo "FATAL: dns.providers.dynu missing from the build" >&2; exit 1; }
"$OUT" list-modules | grep -q "rate_limit" || {
  echo "FATAL: rate_limit missing from the build" >&2; exit 1; }

echo "ok: $OUT"
