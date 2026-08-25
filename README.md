# RASA for Jellyfin

**Remote Access Setup App** — one download that gives a self-hosted Jellyfin server a secure public address, then gets out of the way.

RASA registers a free hostname, obtains a Let's Encrypt certificate, installs a reverse proxy, opens the router port, and writes Jellyfin's own network settings. When it finishes, **you can uninstall RASA and remote access keeps working.**

> **Status: early development.** The project skeleton is in place; the setup wizard is not implemented yet. See [SPEC.md](SPEC.md) for the full design and [Roadmap](#roadmap) for what is built.

---

## Why this exists

Setting up remote access for Jellyfin by hand means a dynamic DNS account, a reverse proxy, an ACME client, a router port forward, and four settings inside Jellyfin that fail silently if you get them wrong. RASA does all of it in about seven minutes, and only asks you for three things: your Jellyfin login, a Dynu account, and a name for your server.

## How it works

RASA is an **installer, not a background app**. It configures three things that keep running on their own:

| Component | Responsibility | Survives RASA's removal |
|---|---|---|
| **Caddy** (installed as a service) | TLS, reverse proxy, and automatic certificate renewal | ✅ |
| **Scheduled task** | Keeps your address pointed at your changing IP | ✅ |
| **Jellyfin settings** | Written once over Jellyfin's own API | ✅ |

Certificates are issued using the **DNS-01 challenge**, so port 80 never needs to be open. If port 443 is unavailable, RASA falls back to 8443 automatically.

If your connection is behind CGNAT, RASA detects it and routes to a mode that still works rather than failing.

## Requirements

- **Jellyfin 10.11.5 or newer**, already installed and running
- **Windows 10/11** or **Linux** (macOS is not supported — see [SPEC.md §17](SPEC.md#17-packaging-and-distribution))
- A free [Dynu](https://www.dynu.com/) account (created during setup)
- Administrator rights, once, during installation

## Installation

Releases are not yet published. When they are, downloads will be on the [Releases](https://github.com/AHouseOfBards/RASA-for-Jellyfin/releases) page.

> ⚠️ **Windows will warn you about this app.** RASA is not code-signed — signing certificates cost money this project does not have. Windows SmartScreen will show *"Windows protected your PC"*; choose **More info → Run anyway**. Verify the SHA-256 checksum published with each release if you want to confirm the download is intact.

## Building from source

```sh
git clone https://github.com/AHouseOfBards/RASA-for-Jellyfin.git
cd RASA-for-Jellyfin

go build ./...
go test ./...
go run ./cmd/rasa -root ./.devdata    # -root avoids needing admin during development
```

That opens the wizard in your browser. Add `-no-browser` to print the address
instead, which is what you want over SSH or in a container.

**Development builds use Let's Encrypt staging.** Production allows five failed
validations per hostname per hour and 50 certificates per registered domain per
week, and a day of debugging against it leaves you locked out of the thing you
are debugging. A release build opts in with `-ldflags "-X main.staging=0"`; a
single run can opt in with `-production-certificates`.

Requires Go 1.26 or newer.

One third-party dependency: `rsc.io/qr`, pinned, with no transitive
dependencies of its own. CI enforces the allowlist — anything else fails the
build. The QR encoder was originally written from scratch to avoid it; that
version round-tripped through its own decoder but disagreed with an established
implementation on the data codewords, and being unable to say which was right
is the whole argument for using the one that phones have already read.

### Layout

```
cmd/rasa/            setup app (disposable)
cmd/rasa-sync/       address sync helper (stays installed)
internal/logging/    structured logging with tested secret redaction
internal/rasaerr/    typed errors: user-facing copy separated from technical detail
internal/state/      resumable setup state machine and its persistence
internal/secrets/    credential storage (DPAPI on Windows, 0600 file on Linux)
internal/paths/      where logs, state and credentials live
internal/probe/      pre-flight: Jellyfin, public address, router/UPnP, ports
internal/mode/       chooses public / IPv6 / mesh access from probe results
internal/dynu/       Dynu v2 API client
internal/portmap/    UPnP IGD port mapping with permanent-lease verification
internal/routerguide/ per-router forwarding instructions from routers.json
internal/reach/      external reachability checks (reachable / unreachable / inconclusive)
internal/dnswait/    waits for records on authoritative nameservers before ACME
internal/jellyfin/   configures Jellyfin's network settings over its own API
internal/domains/    the parent domains on offer, and hostname validation
internal/caddy/      generates the proxy configuration and installs it as a service
internal/service/    registers the OS service and scheduled task
internal/wizard/     the setup flow: sequencing, branching, and the model the UI renders
internal/ui/         serves that model as a local web application
internal/ddns/       keeps the published address current
internal/qr/         renders the finished address for a phone camera
internal/recovery/   recovery file and diagnostic bundle
```

### Live API tests

`go test ./...` is hermetic and needs no network. The Dynu package also has
opt-in read-only tests against the real API, which exist because fixtures can
only prove RASA parses what it was told to expect — they cannot notice Dynu
changing a field name:

```sh
# put your key in .devdata/dynu-key.txt (gitignored), then:
RASA_LIVE_DYNU=1 go test ./internal/dynu/ -run Live -v
```

### Where RASA puts things

These locations deliberately survive uninstallation, because that is when logs matter most.

| | Windows | Linux |
|---|---|---|
| State and recovery file | `C:\ProgramData\RASA` | `/var/lib/rasa` |
| Logs | `C:\ProgramData\RASA\logs` | `/var/log/rasa` |
| Credentials | `C:\ProgramData\RASA\secrets` | `/etc/rasa` |

## Roadmap

Task numbers refer to [SPEC.md §18](SPEC.md#18-implementation-tasks).

- [x] **1** — Project skeleton, state store, structured logging with tested redaction
- [x] **1b** — Error catalogue with user-facing copy
- [x] **2** — Dynu v2 API client
- [x] **3** — Probe suite (Jellyfin discovery, public IP, CGNAT detection)
- [x] **4** — Mode router
- [x] **5** — Port mapper and router instruction guide
- [x] **6** — Caddy service installer
- [x] **7** — DNS propagation waiter
- [x] **8** — Jellyfin configuration client
- [x] **9** — Wizard UI
- [~] **10** — Packaging inputs written; installers not built
- [x] **11** — Scheduled task installer
- [x] **12** — Repair detection, credential reuse, and deliberate removal of remote access
- [x] **13** — Diagnostic bundle and recovery file

## Contributing

Router port-forwarding instructions live in [`internal/routerguide/routers.json`](internal/routerguide/routers.json) rather than in code, so **adding your router is a pull request, not a release**. The same applies to the list of usable Dynu parent domains.

Two rules worth knowing before opening a PR:

1. **Secrets must never reach a log line.** `internal/logging` redacts them and has tests that enforce it. Assume every diagnostic bundle gets pasted into a public issue — because it will.
2. **Users never see raw errors.** Add new failures to `internal/rasaerr`'s catalogue with plain-language copy. There are tests that reject jargon, status codes, and dead ends with no recovery action.

## Security

RASA puts a login form on the public internet, so it takes some responsibility for that: it rate-limits Jellyfin's authentication endpoint, refuses to expose a server with a weak admin password, and never enables Jellyfin's own UPnP.

If you find a security issue, please open an issue marked as such rather than a pull request.

## Licence

Not yet chosen.
