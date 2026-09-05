# RASA for Jellyfin

Watch your Jellyfin server from anywhere.

RASA sets up remote access for you. You run it once, answer three questions, and you get
a web address like `https://yourname.freeddns.org` that works in a browser and in every
Jellyfin app.

When it finishes you can uninstall RASA. Your remote access keeps working without it.

> This is a beta. Please read [Before you rely on it](#before-you-rely-on-it).

## What you need

* Jellyfin 10.11.5 or newer, already installed and running
* Windows 10 or 11, or Linux
* An email address, to make a free account at [Dynu](https://www.dynu.com/) during setup
* Administrator access on the computer, once

macOS is not supported.

## Installing

Download the latest release from the
[Releases page](https://github.com/AHouseOfBards/RASA-for-Jellyfin/releases).

Windows will warn you that the publisher is unknown, because RASA is not code signed.
Click **More info**, then **Run anyway**. Every release lists a SHA-256 checksum if you
want to check the download first.

On Linux, download the `.tar.gz`, unpack it, and run `sudo ./install.sh`.

## What happens during setup

You will be asked for three things: your Jellyfin login, a free Dynu account, and a name
for your server. Everything else happens on its own.

RASA looks at your network, gets your address, sets up a security certificate, asks your
router to let connections in, and updates Jellyfin's own settings. It usually takes about
five minutes, though the certificate step can take longer.

If your router will not open the port by itself, you get step-by-step instructions for
your specific router with the exact values to type.

At the end you get your address, a QR code for your phone, and a text file with everything
written down.

To check it worked, open the address on your phone with Wi-Fi turned off. That is the
only test that proves it works from outside your home.

## Before you rely on it

This is a beta. It has been run start to finish on Windows against a real Jellyfin server,
and it works. Some things have not been tested yet:

* Nobody has run the installer on a clean machine.
* Reaching a server from outside has never been tested successfully. It has only ever run
  on networks where opening a port is not possible.
* Linux has never been tested on real hardware.
* Custom Jellyfin base paths have been tested against a real proxy, but not against a real
  Jellyfin.
* Working out which router you have has not yet recognised a real one. If it cannot tell,
  you can pick yours from a list.

If you try it, please [open an issue](https://github.com/AHouseOfBards/RASA-for-Jellyfin/issues)
and say what happened, whether it worked or not. That is the most useful thing you can do
right now.

## Removing it

Uninstalling RASA leaves your remote access running. Three things it installs stay behind:
a background service that handles the secure connection and renews your certificate, a
scheduled task that keeps your address pointed at your home, and the settings it wrote into
Jellyfin.

To turn remote access off, run RASA again and choose **Remove remote access**. That stops
the service, removes the scheduled task and the firewall rule, and stops your address
pointing at your home. Your Jellyfin server and your logs are left alone.

## If something goes wrong

Everything RASA did is written to a text file when it finishes:
`C:\ProgramData\RASA\remote-access-info.txt` on Windows, or `/var/lib/rasa` on Linux. It
has your address, your port forwarding details, and where to find the logs.

In the same folder, `last-sync.txt` says whether remote access is working now. It is
rewritten every ten minutes and checks that your address still points at your home and
that the secure connection is answering with a valid certificate. On Windows, a real
problem also goes into the Event Log.

The logs are in the same folder. If the security certificate failed, `caddy.log` is the one
to read.

Running `rasa --diagnostics` bundles the logs into a zip file with your address and
passwords taken out, ready to attach to a bug report.

## Building it yourself

You need Go 1.26 or newer.

```sh
git clone https://github.com/AHouseOfBards/RASA-for-Jellyfin.git
cd RASA-for-Jellyfin
go test ./...
go run ./cmd/rasa -root ./.devdata
```

That opens the wizard in your browser and keeps everything in a local folder, so you do not
need administrator rights while developing. [SPEC.md](SPEC.md) explains how it works and
why each decision was made.

## Licence

[GNU General Public License v3.0](LICENSE).

The bundled Caddy binary is Apache-2.0 and `rsc.io/qr` is BSD-3-Clause. Both can be
distributed under GPL-3.0.
