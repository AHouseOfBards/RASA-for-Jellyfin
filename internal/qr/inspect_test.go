package qr

import (
	"os"
	"strings"
	"testing"
)

// TestInspect renders a code as terminal text and writes the PNG, so a
// maintainer can point a real phone at it.
//
// Automated tests can prove the PNG decodes — this output was checked against
// an independent ZXing port and read back correctly — but "a phone camera in a
// room with real lighting" is a different question, and this is how to ask it:
//
//	RASA_QR_SHOW=/tmp/a.png RASA_QR_URL=https://example.freeddns.org //	  go test ./internal/qr/ -run Inspect -v
func TestInspect(t *testing.T) {
	dest := os.Getenv("RASA_QR_SHOW")
	if dest == "" {
		t.Skip("set RASA_QR_SHOW")
	}
	url := os.Getenv("RASA_QR_URL")
	code, err := Encode(url)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for y := -QuietZone; y < code.Size+QuietZone; y += 2 {
		for x := -QuietZone; x < code.Size+QuietZone; x++ {
			top, bottom := code.Dark(x, y), code.Dark(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('\u2588')
			case top:
				b.WriteRune('\u2580')
			case bottom:
				b.WriteRune('\u2584')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	t.Logf("%s (%d modules)\n%s", url, code.Size, b.String())

	png, err := code.PNG(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, png, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("PNG written to %s (%d bytes)", dest, len(png))
}
