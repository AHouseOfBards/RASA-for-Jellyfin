# Packaging

Build and release inputs for RASA. Task 10 of [SPEC.md](../SPEC.md).

Two constraints from the spec shape everything here:

- **No code-signing budget** (decision 13). Windows SmartScreen will warn on
  every download, and reputation never accumulates because it attaches to the
  file hash rather than a publisher. The mitigation is documentation and
  checksums, not a workaround.
- **macOS is out of scope** (decision 7). Distributing outside the App Store
  requires notarization, which requires paid Apple Developer membership.
  Without it Gatekeeper *blocks* rather than warns.

## Contents

| Path | Purpose |
|---|---|
| `caddy/build.sh` | Builds the bundled Caddy with the two required modules |
| `windows/rasa.nsi` | NSIS installer script |
| `windows/rasa.manifest` | Requests administrator, per the journey walkthrough |
| `linux/install.sh` | `curl \| sh` installer — idiomatic on Linux, not on Windows |
| `docker/docker-compose.yml` | The Docker deliverable: a stack, not an installer |

## The Caddy binary is not optional to get right

Stock Caddy ships **neither** module the generated config uses. A stock binary
fails at start with an unrecognised-directive error — *after* RASA has reported
success, which is the worst possible time. `caddy/build.sh` produces the
correct binary and **pins both module versions**; an unpinned build means the
proxy changes underneath users between releases.

## Release checklist

1. `packaging/caddy/build.sh` for each target platform.
2. `go build` RASA and `rasa-sync`, with `-ldflags "-X main.version=$VERSION"`.
3. Verify the bundled Caddy accepts a generated config: `caddy validate`.
4. Build the installers.
5. Publish SHA-256 checksums alongside every asset — with no signature, this is
   the only integrity signal a cautious user has.
6. Update the Pages download page, which does the user-agent detection that
   gives the single-button experience.
