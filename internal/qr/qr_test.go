package qr

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

// The addresses RASA can actually produce must all encode. The longest is a
// 63-character label under the longest parent domain, with a port suffix.
func TestEncodesEveryAddressRASACanProduce(t *testing.T) {
	longestLabel := strings.Repeat("a", 63)
	cases := []string{
		"https://mymedia.freeddns.org",
		"https://mymedia.freeddns.org:8443",
		"https://" + longestLabel + ".webredirect.org",
		"https://" + longestLabel + ".loseyourip.com:8443",
	}
	for _, s := range cases {
		code, err := Encode(s)
		if err != nil {
			t.Fatalf("Encode(%q): %v", s, err)
		}
		if code.Size < 21 {
			t.Errorf("%q produced a %d-module code", s, code.Size)
		}
	}
}

// The quiet zone is the part a hand-rolled renderer forgets, and forgetting it
// produces a code that reads on some devices and not others.
func TestRenderIncludesTheQuietZone(t *testing.T) {
	code, err := Encode("https://mymedia.freeddns.org")
	if err != nil {
		t.Fatal(err)
	}
	const scale = 4
	raw, err := code.PNG(scale)
	if err != nil {
		t.Fatal(err)
	}

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the renderer produced something that is not a PNG: %v", err)
	}

	wantSide := (code.Size + QuietZone*2) * scale
	if b := img.Bounds(); b.Dx() != wantSide || b.Dy() != wantSide {
		t.Fatalf("image is %dx%d, want %dx%d", b.Dx(), b.Dy(), wantSide, wantSide)
	}

	// Every pixel in the margin must be light, on all four sides.
	margin := QuietZone * scale
	for i := 0; i < wantSide; i++ {
		for _, p := range [][2]int{{i, 0}, {i, wantSide - 1}, {0, i}, {wantSide - 1, i}, {i, margin - 1}, {margin - 1, i}} {
			r, g, b, _ := img.At(p[0], p[1]).RGBA()
			if r == 0 && g == 0 && b == 0 {
				t.Fatalf("the quiet zone is dark at (%d,%d)", p[0], p[1])
			}
		}
	}

	// And something inside must be dark, or the "quiet zone" test above would
	// pass on a blank image.
	dark := false
	for y := margin; y < wantSide-margin && !dark; y++ {
		for x := margin; x < wantSide-margin; x++ {
			if r, _, _, _ := img.At(x, y).RGBA(); r == 0 {
				dark = true
				break
			}
		}
	}
	if !dark {
		t.Fatal("the rendered code is blank")
	}
}

// The finder patterns are what a camera locates first. Checking them proves
// the renderer's coordinate mapping is the right way up and not transposed or
// mirrored, which a symmetric test would miss.
func TestFinderPatternsAreWhereTheyBelong(t *testing.T) {
	code, err := Encode("https://mymedia.freeddns.org")
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range [][2]int{{0, 0}, {code.Size - 7, 0}, {0, code.Size - 7}} {
		col, row := origin[0], origin[1]
		for dr := 0; dr < 7; dr++ {
			for dc := 0; dc < 7; dc++ {
				d := max(abs(dc-3), abs(dr-3))
				want := d != 2 && d <= 3
				if got := code.Dark(col+dc, row+dr); got != want {
					t.Fatalf("finder at (%d,%d): module (%d,%d) = %v, want %v", col, row, dc, dr, got, want)
				}
			}
		}
	}
}

func TestDarkIsLightOutsideTheMatrix(t *testing.T) {
	code, err := Encode("hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {code.Size, 0}, {0, code.Size}} {
		if code.Dark(p[0], p[1]) {
			t.Errorf("(%d,%d) is outside the matrix but reported dark", p[0], p[1])
		}
	}
}

// The URI is inlined into the wizard's page, which loads under a policy that
// permits nothing from anywhere else.
func TestDataURI(t *testing.T) {
	uri, err := DataURI("https://mymedia.freeddns.org", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("unexpected prefix: %.40s", uri)
	}
	if len(uri) > 20000 {
		t.Errorf("the data URI is %d bytes, too large to inline comfortably", len(uri))
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
