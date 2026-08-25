#!/usr/bin/env sh
# Installs RASA on Linux, either from an unpacked release tarball or by
# fetching one.
#
# Both paths matter. The tarball ships this script inside it, so the documented
# "unpack it and run sudo ./install.sh" has to install the binaries sitting
# next to the script rather than downloading a second copy of them. Piping this
# from the web is the other idiom this audience expects, and is deliberately
# NOT offered for Windows, where an unsigned script that downloads and executes
# a binary is treated far more harshly by endpoint protection than a plain
# unsigned installer (SPEC.md 17).
set -eu

REPO="AHouseOfBards/RASA-for-Jellyfin"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

# The binaries live together in one directory rather than loose in BIN_DIR,
# with only rasa itself on PATH.
#
# RASA finds the proxy and the sync helper by looking beside its own
# executable, and os.Executable resolves through the symlink to the real path
# here -- so this layout is what makes them discoverable. Installing caddy
# straight into /usr/local/bin was the alternative and is worse twice over: it
# collides with a caddy the user installed themselves, and renaming it out of
# the way (rasa-caddy) leaves RASA unable to find it at all, falling through to
# whatever stock caddy is on PATH. A stock binary has neither module the
# generated config needs.
LIB_DIR="${LIB_DIR:-/usr/local/lib/rasa}"

[ "$(id -u)" -eq 0 ] || { echo "run as root: installing a service needs it" >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

# Where the binaries are coming from. When this script was unpacked from a
# release tarball they are already beside it; $0 is unreliable under a pipe,
# which is exactly the case where there is nothing beside it anyway.
SRC=""
case "$0" in
  */*) candidate=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) ;;
  *)   candidate="" ;;
esac
if [ -n "$candidate" ] && [ -f "$candidate/rasa" ] && [ -f "$candidate/caddy" ]; then
  SRC="$candidate"
  echo "installing from $SRC"
fi

if [ -z "$SRC" ]; then
  command -v curl >/dev/null 2>&1 || { echo "curl is required to download a release" >&2; exit 1; }

  VERSION="${VERSION:-$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')}"
  [ -n "$VERSION" ] || { echo "could not determine the latest version" >&2; exit 1; }

  TARBALL="rasa-${VERSION}-linux-${ARCH}.tar.gz"
  BASE="https://github.com/${REPO}/releases/download/${VERSION}"
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT

  echo "downloading ${VERSION} for linux/${ARCH}"
  curl -fsSL "${BASE}/${TARBALL}" -o "$TMP/$TARBALL"

  # With no code signature, the checksum is the only integrity signal there is,
  # so a missing one is worth saying out loud. The release publishes a single
  # SHA256SUMS.txt covering every artefact; asking for a per-file .sha256 that
  # is never published made this check silently do nothing on every install.
  if curl -fsSL "${BASE}/SHA256SUMS.txt" -o "$TMP/SHA256SUMS.txt"; then
    echo "verifying checksum"
    ( cd "$TMP" && grep " $TARBALL\$" SHA256SUMS.txt | sha256sum -c - )
  else
    echo "warning: no checksums published for this release" >&2
  fi

  tar -xzf "$TMP/$TARBALL" -C "$TMP"
  SRC="$TMP"
fi

for f in rasa rasa-sync caddy; do
  [ -f "$SRC/$f" ] || { echo "missing from the release: $f" >&2; exit 1; }
done

install -d -m 0755 "$LIB_DIR"
install -m 0755 "$SRC/rasa"      "$LIB_DIR/rasa"
install -m 0755 "$SRC/rasa-sync" "$LIB_DIR/rasa-sync"
install -m 0755 "$SRC/caddy"     "$LIB_DIR/caddy"

install -d -m 0755 "$BIN_DIR"
ln -sf "$LIB_DIR/rasa" "$BIN_DIR/rasa"

echo
echo "Installed. Run 'sudo rasa' to set up remote access."
echo "To remove the setup app later: sudo rm -rf $LIB_DIR $BIN_DIR/rasa"
echo "That leaves remote access running. To take it down, run 'sudo rasa' and"
echo "choose Remove remote access first."
