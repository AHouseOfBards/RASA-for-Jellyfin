package caddy

import (
	"strings"
	"testing"
)

// Caddy writes an informational JSON line before it reports anything, so the
// first line of "caddy validate" output is reliably useless. This was found by
// running a real binary, which reported the wrong module for several minutes
// before anyone noticed.
func TestErrorLineSkipsCaddysLogNoise(t *testing.T) {
	out := `{"level":"info","ts":1787609509.75,"msg":"using config from file","file":"C:\x\Caddyfile"}
Error: adapting config using caddyfile: C:/x/Caddyfile:22: unrecognized directive: rate_limit`

	got := errorLine(out)
	if strings.Contains(got, "using config from file") {
		t.Fatalf("errorLine returned the info line: %q", got)
	}
	if !strings.Contains(got, "unrecognized directive") {
		t.Fatalf("errorLine = %q", got)
	}
}

// A missing module has to be named. "Built without a module" sends whoever
// reads it to check the wrong one, which is exactly what happened.
func TestMissingModuleNamesTheRightOne(t *testing.T) {
	cases := []struct {
		detail    string
		directive string
		module    string
	}{
		{"Error: adapting config using caddyfile: /x:22: unrecognized directive: rate_limit", "rate_limit", "caddy-ratelimit"},
		{"Error: adapting config using caddyfile: /x:12: unrecognized directive: dynu", "dynu", "caddy-dns/dynu"},
		{"Error: loading module: getting module named dns.providers.dynu", "dns dynu", "caddy-dns/dynu"},
	}
	for _, tc := range cases {
		directive, module, ok := missingModule(tc.detail)
		if !ok {
			t.Errorf("%q was not recognised as a missing module", tc.detail)
			continue
		}
		if directive != tc.directive || module != tc.module {
			t.Errorf("%q gave (%q, %q), want (%q, %q)", tc.detail, directive, module, tc.directive, tc.module)
		}
	}

	if _, _, ok := missingModule("Error: /x:3: unexpected token '}'"); ok {
		t.Error("an ordinary syntax error was reported as a missing module")
	}
}
