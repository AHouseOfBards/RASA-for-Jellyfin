package ui

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/wizard"
)

func asset(t *testing.T, name string) string {
	t.Helper()
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// The wizard is a static page and a script with no build step and no framework,
// which is deliberate — but it means a renamed element is not an error
// anywhere. It is a null dereference at the moment a user reaches the screen,
// which for half these ids is a screen only reached when something has already
// gone wrong.
func TestEveryElementTheScriptReachesForExists(t *testing.T) {
	js, html := asset(t, "app.js"), asset(t, "index.html")

	ids := regexp.MustCompile(`getElementById\("([^"]+)"\)`).FindAllStringSubmatch(js, -1)
	if len(ids) < 20 {
		t.Fatalf("found only %d element lookups; the pattern has probably stopped matching", len(ids))
	}

	var missing []string
	seen := map[string]bool{}
	for _, m := range ids {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if !strings.Contains(html, `id="`+id+`"`) {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the script reaches for elements that are not in the page: %v", missing)
	}
}

// Every screen the model can name has to have somewhere to render. A screen
// with no section shows a blank page.
func TestEveryScreenTheScriptShowsHasASection(t *testing.T) {
	js, html := asset(t, "app.js"), asset(t, "index.html")

	var missing []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bshow\("([a-z-]+)"\)`).FindAllStringSubmatch(js, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(html, `data-screen="`+name+`"`) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the script shows screens that do not exist: %v", missing)
	}
}

// A problem box that is always present in the page must collapse when empty,
// or the name picker carries an empty bordered card on every visit.
func TestAnEmptyProblemBoxIsHidden(t *testing.T) {
	css := asset(t, "app.css")
	if !strings.Contains(css, ".problem:empty") {
		t.Error("app.css does not hide an empty .problem, so a screen that hosts one shows an empty card")
	}
}

// These two ids are built by concatenation rather than written out, so the
// check above cannot see them — and they are on the path that renders every
// failure the product has.
func TestEveryScreenThatHostsAProblemHasBothItsBoxes(t *testing.T) {
	js, html := asset(t, "app.js"), asset(t, "index.html")

	m := regexp.MustCompile(`PROBLEM_SCREENS = \[([^\]]*)\]`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("PROBLEM_SCREENS is gone; renderProblem no longer declares which screens host a problem box")
	}
	names := regexp.MustCompile(`"([a-z-]+)"`).FindAllStringSubmatch(m[1], -1)
	if len(names) == 0 {
		t.Fatal("PROBLEM_SCREENS is empty, so no failure can be rendered anywhere")
	}
	for _, n := range names {
		for _, suffix := range []string{"-problem", "-problem-actions"} {
			id := n[1] + suffix
			if !strings.Contains(html, `id="`+id+`"`) {
				t.Errorf("screen %q hosts a problem box but the page has no %s", n[1], id)
			}
		}
		if !strings.Contains(html, `data-screen="`+n[1]+`"`) {
			t.Errorf("screen %q hosts a problem box but is not a screen", n[1])
		}
	}
}

// The token is what stops another page on the machine driving setup. It has to
// be in the document for the script to find, and it must not be in the asset
// on disk.
func TestTheTokenIsSubstitutedRatherThanShipped(t *testing.T) {
	html := asset(t, "index.html")
	if !strings.Contains(html, `name="rasa-token"`) {
		t.Error("the token meta tag is missing; the script cannot authenticate any request")
	}
	for _, leak := range []string{"content=\"rasa", "content=\"tok"} {
		if strings.Contains(strings.ToLower(html), leak) {
			t.Errorf("the shipped page appears to contain a literal token: %q", leak)
		}
	}
}

// The script reads the port view field by field, by name, with no build step
// and nothing that would notice a rename. A field renamed in Go becomes
// undefined in the browser, and undefined renders as a hidden element or a
// blank line rather than an error — on the one screen that exists because
// something has already gone wrong.
func TestThePortScreenOnlyReadsFieldsThePortViewSends(t *testing.T) {
	js := asset(t, "app.js")

	tags := map[string]bool{}
	v := reflect.TypeOf(wizard.PortView{})
	for i := 0; i < v.NumField(); i++ {
		tag := v.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		tags[strings.Split(tag, ",")[0]] = true
	}
	if len(tags) < 10 {
		t.Fatalf("PortView has only %d json fields; the reflection has probably stopped working", len(tags))
	}

	reads := regexp.MustCompile(`\bp\.([a-z_]+)\b`).FindAllStringSubmatch(js, -1)
	if len(reads) < 10 {
		t.Fatalf("found only %d port field reads; the pattern has probably stopped matching", len(reads))
	}

	var unknown []string
	seen := map[string]bool{}
	for _, m := range reads {
		f := m[1]
		if seen[f] {
			continue
		}
		seen[f] = true
		if !tags[f] {
			unknown = append(unknown, f)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("the port screen reads fields the wizard never sends: %v", unknown)
	}
}
