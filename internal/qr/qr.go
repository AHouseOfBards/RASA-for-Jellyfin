// Package qr renders RASA's finished address as a QR code.
//
// SPEC.md §9 step 12 hands the address over with Copy, QR and Open. The QR
// matters more than it looks: the thing a user most wants to do with a new
// address is check it from a phone on mobile data, which is also the only
// check that proves the setup works from outside the house. Typing
// "https://mymedia.freeddns.org" into a phone keyboard is exactly the friction
// that stops people doing it.
//
// # Why this uses a library
//
// The encoder here was originally written from scratch, to preserve a
// zero-dependency build. That was a mistake and it is worth recording why.
// Nothing in the spec required zero dependencies — §18 explicitly anticipated
// one — and the from-scratch encoder, while it round-tripped through its own
// decoder and produced format bits matching the published standard, disagreed
// with an established implementation on the data codewords for every input.
// Being unable to say which was right is the whole argument: this feature
// exists to be read by an unknown phone camera, so the implementation that has
// been read by millions of them wins.
//
// rsc.io/qr is pinned, has no dependencies of its own beyond its subpackages,
// and is small enough to read. The rendering below is ours because the quiet
// zone and scale are decisions worth making explicitly.
package qr

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"

	"rsc.io/qr"
)

// DefaultScale is how many image pixels wide one module is rendered. Six is
// enough for a phone camera to read comfortably off a screen without producing
// a file too large to inline in a page.
const DefaultScale = 6

// QuietZone is the light margin, in modules, around the symbol.
//
// Four is what the standard requires, and it is not decoration: without it a
// reader cannot find the symbol's edge against whatever is behind it. Omitting
// it is the most common way a QR ends up readable on some devices and not
// others, so it is rendered here rather than left to the caller's layout.
const QuietZone = 4

// Level is the error correction level. M tolerates about 15% damage, which is
// the usual choice for a code read off a screen rather than a printed label
// that might get scuffed.
const level = qr.M

// Code is a rendered QR matrix.
type Code struct {
	// Size is the width and height in modules, excluding the quiet zone.
	Size int

	code *qr.Code
}

// Encode builds a QR code for s.
func Encode(s string) (*Code, error) {
	c, err := qr.Encode(s, level)
	if err != nil {
		return nil, fmt.Errorf("encoding %d characters as a QR code: %w", len(s), err)
	}
	return &Code{Size: c.Size, code: c}, nil
}

// Dark reports whether the module at (x, y) is dark. Coordinates outside the
// matrix are light, which makes the quiet zone fall out of the renderer rather
// than needing a special case.
func (c *Code) Dark(x, y int) bool {
	if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
		return false
	}
	return c.code.Black(x, y)
}

// PNG renders the code as a monochrome PNG.
//
// Black on white regardless of the viewer's theme. A QR inverted for a dark
// background is readable by some scanners and not others, and this one exists
// to be pointed at by an unknown phone.
func (c *Code) PNG(scale int) ([]byte, error) {
	if scale <= 0 {
		scale = DefaultScale
	}
	side := (c.Size + QuietZone*2) * scale

	img := image.NewPaletted(image.Rect(0, 0, side, side), color.Palette{
		color.White,
		color.Black,
	})
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			if c.Dark(x/scale-QuietZone, y/scale-QuietZone) {
				img.SetColorIndex(x, y, 1)
			}
		}
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding the QR image: %w", err)
	}
	return buf.Bytes(), nil
}

// DataURI renders the code as a data: URI, ready to drop into an img element.
//
// Inline rather than a served endpoint because the wizard's page loads under a
// policy that permits nothing from anywhere else, and because the address it
// encodes should not need a second request to appear.
func DataURI(s string, scale int) (string, error) {
	code, err := Encode(s)
	if err != nil {
		return "", err
	}
	raw, err := code.PNG(scale)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw), nil
}
