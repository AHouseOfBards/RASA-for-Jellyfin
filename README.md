# RASA for Jellyfin

Watch your Jellyfin server from anywhere, without setting up a dozen things by hand.

RASA is a small setup app. You run it once, answer three questions, and it does the rest:
it gets you a free web address, sets up a proper security certificate so browsers trust it,
tells your router to let connections in, and updates Jellyfin's own settings.

When it's done you can delete RASA. Your remote access keeps working.

> **This is a 0.1 beta.** It has been run end to end on Windows and it works, but it's new.
> Read [What isn't proven yet](#what-isnt-proven-yet) before you rely on it.

## What you get

An address like `https://yourname.freeddns.org` that works in a browser and in every
Jellyfin app, from anywhere.

Doing this by hand normally means signing up for a dynamic DNS service, installing a
reverse proxy, running an ACME client to get a certificate, forwarding a port on your
router, and changing four settings inside Jellyfin that fail quietly if you get them
wrong. RASA does all of that in about ten minutes.

## What you need

- Jellyfin 10.11.5 or newer, already installed and running
- Windows 10/11, or Linux
- An email address, to make a free account at [Dynu](https://www.dynu.com/) during setup
- Administrator access on the computer, once

macOS isn't supported. Apple requires a paid developer account to distribute apps outside
their store, and without it macOS blocks the app rather than just warning about it.

## Installing

Download the latest release from the
[Releases page](https://github.com/AHouseOfBards/RASA-for-Jellyfin/releases).

**Windows will warn you about this app.** You'll see *"Windows protected your PC"*. That's
because the app isn't code-signed, which costs money this project doesn't have yet. It
isn't a sign that anything is wrong, but you only have our word for that. Click
**More info**, then **Run anyway**. If you'd rather check first, every release publishes a
SHA-256 checksum you can compare against your download.

On Linux, download the `.tar.gz`, unpack it, and run `sudo ./install.sh`.

## What happens during setup

You'll be asked for three things: your Jellyfin admin login, a Dynu account, and a name
for your server. Everything else is automatic.

1. RASA checks your network and finds your Jellyfin server
2. You sign in to Jellyfin so RASA can change its settings
3. You make a free Dynu account and paste in a key it gives you
4. You pick a name, like `mymedia`
5. RASA asks your router to open a port. If your router won't, you get exact
   instructions for your specific router model
6. RASA sets everything up. **The security certificate can take up to nine minutes** —
   that step is slow because a certificate authority has to check your address is really
   yours. The screen keeps counting so you can tell it's still working
7. You get your address, a QR code, and a text file with everything written down

The best way to check it worked is to open the address on your phone with wi-fi turned
off. That's the only test that proves it works from outside your home.

## What stays on your computer

RASA itself is disposable, but three things it installs are not:

| What | Why it has to stay |
| --- | --- |
| Caddy, as a background service | Handles the secure connection and renews your certificate |
| A scheduled task | Your home address changes; this keeps your web address pointed at it |
| Jellyfin's settings | Written once, so Jellyfin knows its new address |

Uninstalling RASA leaves all three alone, on purpose. That's what lets you delete the
setup app without breaking anything.

## Removing it

To take remote access down properly, run RASA again and choose **Remove remote access**.
That stops the service, removes the scheduled task and firewall rule, and stops your web
address pointing at your home.

Your Jellyfin server itself is never touched, and your logs are left in place.

## If something goes wrong

Everything RASA did is written to a text file when it finishes. On Windows that's
`C:\ProgramData\RASA\remote-access-info.txt`, and on Linux `/var/lib/rasa`. It has your
address, your port forwarding details, and where the logs are.

Logs live next to it:

| File | What's in it |
| --- | --- |
| `rasa.log` | What the setup app did |
| `caddy.log` | The secure connection and certificate — **read this if a certificate fails** |
| `sync.log` | Address updates |

You can also run `rasa --diagnostics`, which bundles the logs into a zip with your address
and any secrets removed, ready to attach to a bug report.

## What isn't proven yet

Being straight about this, because it's a beta:

- **Reaching your server from outside has never been tested successfully.** Everything
  else has been run end to end on a real machine, but testing was done on a network with
  no consumer router, where opening a port isn't possible. The code paths exist and fail
  gracefully; they've never succeeded.
- **Nobody has run the installers yet.** They build, and you can check the checksum
  against what's published, but no one has yet double-clicked the `.exe` on a machine
  that didn't already have RASA on it.
- Only Windows has been tested on real hardware. Linux compiles and is covered by tests,
  but the Linux install script has never been run.

If you try it, [open an issue](https://github.com/AHouseOfBards/RASA-for-Jellyfin/issues)
and say what happened either way. That's the most useful thing you can do right now.

## Building from source

Requires Go 1.26 or newer.

```sh
git clone https://github.com/AHouseOfBards/RASA-for-Jellyfin.git
cd RASA-for-Jellyfin
go build ./...
go test ./...
go run ./cmd/rasa -root ./.devdata
```

That opens the wizard in your browser. `-root` keeps everything in a local folder so you
don't need administrator rights while developing. Add `-no-browser` to print the address
instead, which is what you want over SSH.

Development builds use Let's Encrypt's **staging** service. Production allows only five
failed attempts per address per hour, and a day of debugging against it locks you out of
the thing you're debugging. Release builds opt in with `-ldflags "-X main.staging=0"`.

There's one third-party dependency, `rsc.io/qr`, pinned, for the QR code. CI fails the
build if anything else appears.

[SPEC.md](SPEC.md) is the full design document, including why each decision was made.

## Licence

[GNU General Public License v3.0](LICENSE).

The bundled Caddy binary is Apache-2.0 and `rsc.io/qr` is BSD-3-Clause; both are
compatible with distribution under GPL-3.0.
