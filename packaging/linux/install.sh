#!/usr/bin/env sh
# curl | sh installer for Linux.
#
# Idiomatic here and expected by this audience. Deliberately NOT offered for
# Windows, where an unsigned script that downloads and executes a binary is
# treated far more harshly by endpoint protection than a plain unsigned
# installer (SPEC.md 17).
set -eu

REPO="AHouseOfBards/RASA-for-Jellyfin"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

[ "$(id -u)" -eq 0 ] || { echo "run as root: installing a service needs it" >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

VERSION="${VERSION:-$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')}"
[ -n "$VERSION" ] || { echo "could not determine the latest version" >&2; exit 1; }

TARBALL="rasa-${VERSION}-linux-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "downloading ${VERSION} for linux/${ARCH}"
curl -fsSL "$URL" -o "$TMP/$TARBALL"

# With no code signature, the checksum is the only integrity signal available.
if curl -fsSL "${URL}.sha256" -o "$TMP/sum" 2>/dev/null; then
  echo "verifying checksum"
  ( cd "$TMP" && sed "s|  .*|  ${TARBALL}|" sum | sha256sum -c - )
else
  echo "warning: no checksum published for this release" >&2
fi

tar -xzf "$TMP/$TARBALL" -C "$TMP"
install -m 0755 "$TMP/rasa"      "${INSTALL_DIR}/rasa"
install -m 0755 "$TMP/rasa-sync" "${INSTALL_DIR}/rasa-sync"
install -m 0755 "$TMP/caddy"     "${INSTALL_DIR}/rasa-caddy"

echo
echo "Installed. Run 'sudo rasa' to set up remote access."
