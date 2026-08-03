package fleetqr

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

// quietZone is the mandatory light margin. Scanners use it to find the code's
// edge; without it a QR flush against a dark UI often will not read at all.
const quietZone = 4

// level M rather than L: this code is photographed off a glossy screen, at an
// angle, by a hand. The payload is small enough that the extra recovery data
// costs a version at most.
const level = qr.M

// Encode builds the code for a payload. Kept separate from the renderers so
// both draw exactly the same modules.
func Encode(payload string) (*qr.Code, error) {
	code, err := qr.Encode(payload, level)
	if err != nil {
		return nil, fmt.Errorf("fleetqr: encode: %w", err)
	}
	return code, nil
}

// SVG draws the code as scalable vector output. Vector rather than PNG because
// the sheet showing this can be any size on any display density, and a QR that
// resamples badly is a QR that will not scan.
//
// Colours are explicit and fixed: dark modules on a light field, never themed.
// A QR inverted for a dark UI is a QR many scanners refuse, so the tile stays
// light even when the surface around it is dark.
func SVG(payload string) ([]byte, error) {
	code, err := Encode(payload)
	if err != nil {
		return nil, err
	}
	span := code.Size + quietZone*2

	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" `+
		`shape-rendering="crispEdges" role="img" aria-label="Código de vinculación">`, span, span)
	fmt.Fprintf(&out, `<rect width="%d" height="%d" fill="#ffffff"/>`, span, span)
	out.WriteString(`<path fill="#000000" d="`)
	// One path of horizontal runs rather than a rect per module: a version-6
	// code is 1681 modules, and runs cut that to a few hundred segments.
	for y := 0; y < code.Size; y++ {
		runStart := -1
		for x := 0; x <= code.Size; x++ {
			dark := x < code.Size && code.Black(x, y)
			switch {
			case dark && runStart < 0:
				runStart = x
			case !dark && runStart >= 0:
				fmt.Fprintf(&out, "M%d %dh%dv1H%dz", runStart+quietZone, y+quietZone, x-runStart, runStart+quietZone)
				runStart = -1
			}
		}
	}
	out.WriteString(`"/></svg>`)
	return []byte(out.String()), nil
}

// Terminal draws the code with half-block characters so a headless machine can
// show one over SSH. Two module rows per text line keeps it roughly square and
// small enough for an ordinary terminal.
//
// Foreground and background are set explicitly on every cell rather than
// inherited: a terminal with a dark theme would otherwise render the code
// inverted, and inverted codes are read by some scanners and refused by others.
func Terminal(payload string) (string, error) {
	code, err := Encode(payload)
	if err != nil {
		return "", err
	}
	const (
		reset = "\x1b[0m"
		light = 15 // bright white
		dark  = 0  // black
	)
	shade := func(x, y int) int {
		// Anything outside the module grid is quiet zone, which is light.
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return light
		}
		if code.Black(x, y) {
			return dark
		}
		return light
	}

	var out strings.Builder
	for y := -quietZone; y < code.Size+quietZone; y += 2 {
		for x := -quietZone; x < code.Size+quietZone; x++ {
			// The glyph is an upper half block, so its colour is the top module
			// and the cell background is the one below it.
			fmt.Fprintf(&out, "\x1b[38;5;%d;48;5;%dm▀", shade(x, y), shade(x, y+1))
		}
		out.WriteString(reset + "\n")
	}

	return out.String(), nil
}
