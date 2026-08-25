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
#
# Releases are cross-compiled -- every target is built on a linux/amd64 runner
# -- so `list-modules` cannot be the only check: asking the host to execute a
# windows/arm64 binary fails for reasons that have nothing to do with the
# modules. The string check works on any target because a module ID is a
# compile-time constant that is only in the binary if the module was linked in,
# and it runs for native builds too rather than being the cross-compile excuse.
required="dns.providers.dynu http.handlers.rate_limit"

echo "verifying required modules are linked in"
for id in $required; do
  grep -qa "$id" "$OUT" || {
    echo "FATAL: $id is missing from the build" >&2; exit 1; }
done

if [ "$(go env GOOS)/$(go env GOARCH)" = "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]; then
  echo "verifying the binary registers them"
  for id in $required; do
    "$OUT" list-modules | grep -q "$id" || {
      echo "FATAL: $id is not registered by the built binary" >&2; exit 1; }
  done
else
  echo "cross-compiled: skipping the execution check"
fi

echo "ok: $OUT"
