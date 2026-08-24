package caddy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedConfigAgainstRealCaddy runs `caddy validate` over what Generate
// produces.
//
// The rest of this package's tests compare strings, which proves the generator
// is consistent with itself and nothing more. Caddy is the only authority on
// whether the file is valid, and the specific failure worth catching cannot be
// caught any other way: a stock Caddy parses every line except the one that
// needs caddy-dns/dynu, so a build without that module produces a proxy that
// starts, reports success, and then cannot answer a certificate challenge.
//
// Gated on a binary being available, so "go test ./..." stays hermetic:
//
//	RASA_CADDY_BINARY=./dist/caddy go test ./internal/caddy/ -run RealCaddy -v
//
// The Audit workflow builds one with packaging/caddy/build.sh and points this
// at it, which is what turns "the config looks right" into "Caddy accepts it".
func TestGeneratedConfigAgainstRealCaddy(t *testing.T) {
	binary := os.Getenv("RASA_CADDY_BINARY")
	if binary == "" {
		if p, err := exec.LookPath(BinaryName()); err == nil {
			binary = p
		} else {
			t.Skip("set RASA_CADDY_BINARY to a caddy built with packaging/caddy/build.sh")
		}
	}

	dir := t.TempDir()
	in := &Installer{
		BinaryPath:    binary,
		CaddyfilePath: filepath.Join(dir, "Caddyfile"),
		DataDir:       dir,
	}

	cases := map[string]Config{
		"typical": {
			Hostname:        "mymedia.freeddns.org",
			ListenPort:      443,
			UpstreamAddress: "127.0.0.1:8096",
			DynuAPIKeyEnv:   "RASA_DYNU_TOKEN",
			OwnDomain:       "mymedia.freeddns.org",
			LogPath:         filepath.Join(dir, "caddy.log"),
			ACMECA:          ACMEStaging,
		},
		"fallback port and a base path": {
			Hostname:        "mymedia.kozow.com",
			ListenPort:      8443,
			UpstreamAddress: "192.168.1.50:8920",
			BaseURL:         "/jellyfin",
			DynuAPIKeyEnv:   "RASA_DYNU_TOKEN",
			OwnDomain:       "mymedia.kozow.com",
			LogPath:         filepath.Join(dir, "caddy.log"),
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			text, err := cfg.Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if err := os.WriteFile(in.CaddyfilePath, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}

			err = in.Validate(context.Background(), map[string]string{"RASA_DYNU_TOKEN": "not-a-real-token"})
			if err == nil {
				return
			}
			if strings.Contains(err.Error(), "the bundled proxy is missing") {
				t.Fatalf("this Caddy was built without a module the generated file needs, "+
					"which is the exact packaging failure this test exists to catch. "+
					"Build one with packaging/caddy/build.sh.\n%v", err)
			}
			t.Fatalf("Caddy rejected the generated configuration:\n%v\n\n%s", err, text)
		})
	}
}
