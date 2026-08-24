package domains

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// PSLURL is the authoritative list. It is fetched rather than vendored because
// the point of this test is to notice it changing.
const PSLURL = "https://publicsuffix.org/list/public_suffix_list.dat"

// TestAgainstLivePublicSuffixList is the audit SPEC.md §8 asks CI to run.
//
// It is what found the three unsafe Dynu domains in the first place, and it
// exists as a standing check because the failure it prevents is invisible:
// a parent domain that leaves the Public Suffix List does not break anything
// today. It quietly merges every RASA user on it into one Let's Encrypt
// certificate bucket, and issuance starts failing weeks later for reasons no
// individual user can see.
//
// Network-gated, so "go test ./..." stays hermetic:
//
//	RASA_AUDIT_PSL=1 go test ./internal/domains/ -run PublicSuffix -v
func TestAgainstLivePublicSuffixList(t *testing.T) {
	if os.Getenv("RASA_AUDIT_PSL") != "1" {
		t.Skip("set RASA_AUDIT_PSL=1 to fetch the live Public Suffix List")
	}

	suffixes, err := fetchPSL(t)
	if err != nil {
		t.Fatalf("fetching the Public Suffix List: %v", err)
	}
	if len(suffixes) < 5000 {
		// A truncated or redirected response would otherwise fail every domain
		// at once and read as a catastrophe rather than a bad download.
		t.Fatalf("the list looks wrong: only %d entries", len(suffixes))
	}

	c, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range c.All() {
		if !suffixes[d.Name] {
			t.Errorf("%s is offered to users but is NOT on the Public Suffix List.\n"+
				"Every RASA user on it would share one Let's Encrypt certificate bucket.\n"+
				"Remove it from domains.json and move it to the blocked list.", d.Name)
		}
	}

	// The other direction is a finding, not a failure: a domain that has since
	// been listed could safely be offered, and nobody would otherwise notice.
	for name := range c.blocked {
		if suffixes[name] {
			t.Logf("note: %s is now on the Public Suffix List and could be offered", name)
		}
	}
}

func fetchPSL(t *testing.T) (map[string]bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, PSLURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errStatus(res.StatusCode)
	}

	out := make(map[string]bool, 10000)
	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Comments start with //. Wildcard (*.) and exception (!) rules are
		// irrelevant here: RASA only ever asks about exact parent domains.
		if line == "" || strings.HasPrefix(line, "//") ||
			strings.HasPrefix(line, "*") || strings.HasPrefix(line, "!") {
			continue
		}
		out[strings.ToLower(line)] = true
	}
	return out, scanner.Err()
}

type errStatus int

func (e errStatus) Error() string {
	return "unexpected status " + http.StatusText(int(e))
}
