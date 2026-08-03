package fleetqr

import (
	"fmt"
	"strings"
	"testing"

	"workass/internal/fleet"
)

const testKey = "wf-dxav2wvjok3kj6hmir7ymwdj5y"

// The library draws a correct QR; what it cannot check is that we put the right
// string inside it. A payload that encodes perfectly and enrols nowhere is the
// bug this round-trip exists to catch.
func TestJoinPayloadRoundTripsThroughItsOwnParser(t *testing.T) {
	cases := []struct {
		name string
		join Join
		want Join
	}{
		{
			name: "default port stays out of the code",
			join: Join{Host: "192.168.0.13", Key: testKey, Name: "dev-MacBook-Pro"},
			want: Join{Host: "192.168.0.13", Port: DefaultPort, Key: testKey, Name: "dev-MacBook-Pro"},
		},
		{
			name: "a machine on another port carries it",
			join: Join{Host: "192.168.1.50", Port: 18788, Key: testKey, Name: "builder"},
			want: Join{Host: "192.168.1.50", Port: 18788, Key: testKey, Name: "builder"},
		},
		{
			name: "the cosmetic name is optional",
			join: Join{Host: "192.168.0.13", Key: testKey},
			want: Join{Host: "192.168.0.13", Port: DefaultPort, Key: testKey},
		},
		{
			name: "a key typed in any shape becomes canonical",
			join: Join{Host: "192.168.0.13", Key: "WF-DXAV 2WVJ-OK3K J6HM IR7Y MWDJ5Y"},
			want: Join{Host: "192.168.0.13", Port: DefaultPort, Key: testKey},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload, err := BuildURL(testCase.join)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if !strings.HasPrefix(payload, Scheme+"://"+JoinPath+"?") {
				t.Fatalf("payload does not start with the scheme the scanner matches on: %q", payload)
			}
			back, err := ParseURL(payload)
			if err != nil {
				t.Fatalf("parse %q: %v", payload, err)
			}
			if back != testCase.want {
				t.Fatalf("round trip changed the join\n payload %q\n got  %+v\n want %+v", payload, back, testCase.want)
			}
			// And it has to survive the encoder at the level we ship.
			if _, err := Encode(payload); err != nil {
				t.Fatalf("encode %q (%d bytes): %v", payload, len(payload), err)
			}
		})
	}
}

// The scanner and this builder live in separate repos and will change at
// different times. Ignoring unknown parameters is what lets one of them add a
// field without bricking a code already sitting in someone's pocket.
func TestParseIgnoresParametersItDoesNotKnow(t *testing.T) {
	payload := Scheme + "://" + JoinPath + "?h=192.168.0.13&k=" + testKey + "&n=Mac&v=7&future=whatever"
	join, err := ParseURL(payload)
	if err != nil {
		t.Fatalf("a future field made an otherwise valid code unreadable: %v", err)
	}
	if join.Host != "192.168.0.13" || join.Key != testKey || join.Name != "Mac" {
		t.Fatalf("join = %+v", join)
	}
}

// Each of these scans perfectly and then fails at enrolment, which is the worst
// possible place to find out. Refuse at build time instead.
func TestBuildRefusesCodesThatCannotEnrol(t *testing.T) {
	cases := []struct {
		name string
		join Join
		want string
	}{
		{"no address", Join{Key: testKey}, "address is required"},
		{"loopback v4", Join{Host: "127.0.0.1", Key: testKey}, "loopback"},
		{"loopback v6", Join{Host: "::1", Key: testKey}, "loopback"},
		{"localhost", Join{Host: "localhost", Key: testKey}, "loopback"},
		{"unspecified", Join{Host: "0.0.0.0", Key: testKey}, "loopback"},
		{"no key", Join{Host: "192.168.0.13"}, "fleetqr:"},
		{"truncated key", Join{Host: "192.168.0.13", Key: "wf-dxav2wvj"}, "fleetqr:"},
		{"port out of range", Join{Host: "192.168.0.13", Port: 70000, Key: testKey}, "out of range"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload, err := BuildURL(testCase.join)
			if err == nil {
				t.Fatalf("built an unusable code: %q", payload)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

func TestParseRejectsWhatIsNotAJoinCode(t *testing.T) {
	for _, payload := range []string{
		"https://example.com/join?h=192.168.0.13&k=" + testKey,
		"workass://machines?h=192.168.0.13&k=" + testKey,
		"workass://join?k=" + testKey,                     // a bare key cannot start a join
		"workass://join?h=127.0.0.1&k=" + testKey,         // the phone would dial itself
		"workass://join?h=192.168.0.13&k=not-a-fleet-key", // scans, never enrols
		"workass://join?h=192.168.0.13:0&k=" + testKey,
	} {
		if join, err := ParseURL(payload); err == nil {
			t.Fatalf("accepted %q as %+v", payload, join)
		}
	}
}

// A real key at a realistic address must fit the code we actually draw. This is
// the guard that a longer machine name or a hostname instead of an IP has not
// quietly pushed the payload past the version the renderers assume.
func TestTheLongestRealisticPayloadStillEncodes(t *testing.T) {
	key, err := fleet.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := BuildURL(Join{
		Host: "dev-macbook-pro-16-inch.local",
		Port: 18788,
		Key:  key,
		Name: "dev-MacBook-Pro-16-inch",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := Encode(payload)
	if err != nil {
		t.Fatalf("encode %d-byte payload: %v", len(payload), err)
	}
	// Density, not format complexity, is the ceiling that matters now that a
	// library draws this: every extra module is smaller on screen and harder to
	// photograph. 45x45 is version 7, which a phone reads comfortably at the size
	// a sheet shows it.
	if code.Size > 45 {
		t.Fatalf("payload of %d bytes needs a %dx%d code, dense enough to hurt scanning: %q",
			len(payload), code.Size, code.Size, payload)
	}
}

// The cosmetic name is the only field a human can make arbitrarily long, and
// every byte of it makes the code denser for no benefit — the phone overwrites
// it from /workass/health on arrival.
func TestALongMachineNameCannotInflateTheCode(t *testing.T) {
	payload, err := BuildURL(Join{
		Host: "192.168.0.13",
		Key:  testKey,
		Name: strings.Repeat("nombre larguísimo de máquina ", 6),
	})
	if err != nil {
		t.Fatal(err)
	}
	join, err := ParseURL(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len([]rune(join.Name)) > nameLimit {
		t.Fatalf("name survived at %d runes: %q", len([]rune(join.Name)), join.Name)
	}
	code, err := Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if code.Size > 45 {
		t.Fatalf("a long name still reached %dx%d", code.Size, code.Size)
	}
}

// A developer machine answers on several private addresses that are
// indistinguishable by IP: a VPN on 10.x, a tailnet on 100.64/10, a VM bridge on
// 192.168.64/24, and the LAN the phone is actually on. Picking the wrong one
// produces a code that scans perfectly and connects to nothing — the exact
// failure this ordering exists to prevent. These are the real interfaces of the
// machine this was built on.
func TestTheLanBeatsEveryTunnelAndBridge(t *testing.T) {
	ranked := RankCandidates([]Candidate{
		{Host: "10.5.5.2", Iface: "utun6"},
		{Host: "192.168.64.1", Iface: "bridge100"},
		{Host: "100.64.0.3", Iface: "utun10"},
		{Host: "192.168.0.13", Iface: "en0"},
	})
	if len(ranked) != 4 || ranked[0].Host != "192.168.0.13" {
		t.Fatalf("ranked %+v, want the en0 LAN address first", ranked)
	}
	// Nothing is dropped: a tailnet address is the right answer for someone whose
	// phone is on the tailnet, so it stays offerable — just never the default.
	seen := map[string]bool{}
	for _, candidate := range ranked {
		seen[candidate.Host] = true
	}
	if len(seen) != 4 {
		t.Fatalf("ranking dropped candidates: %+v", ranked)
	}
}

func TestRankingPrefersOrdinaryInterfacesOnLinuxAndWindowsToo(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		candidates []Candidate
		want       string
	}{
		{"linux wifi over docker", []Candidate{
			{Host: "172.17.0.1", Iface: "docker0"},
			{Host: "192.168.0.50", Iface: "wlan0"},
		}, "192.168.0.50"},
		{"linux predictable names", []Candidate{
			{Host: "10.8.0.2", Iface: "wg0"},
			{Host: "192.168.1.7", Iface: "enp3s0"},
		}, "192.168.1.7"},
		{"an unknown interface still beats a tunnel", []Candidate{
			{Host: "10.5.5.2", Iface: "utun6"},
			{Host: "192.168.7.7", Iface: "mystery0"},
		}, "192.168.7.7"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ranked := RankCandidates(testCase.candidates)
			if ranked[0].Host != testCase.want {
				t.Fatalf("ranked %+v, want %s first", ranked, testCase.want)
			}
		})
	}
}

// The SVG is a hand-drawn re-rendering of the library's module grid, so it is
// the one part of the pipeline a decoder cannot vouch for: a run-length bug
// would produce a plausible-looking image of the wrong code. This reconstructs
// the grid from the emitted path and compares it module for module.
func TestTheSVGDrawsExactlyTheModulesTheEncoderProduced(t *testing.T) {
	payload, err := BuildURL(Join{Host: "192.168.0.13", Port: 18788, Key: testKey, Name: "dev-MacBook-Pro"})
	if err != nil {
		t.Fatal(err)
	}
	code, err := Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	svg, err := SVG(payload)
	if err != nil {
		t.Fatal(err)
	}

	drawn := map[[2]int]bool{}
	body := string(svg)
	start := strings.Index(body, `d="`)
	if start < 0 {
		t.Fatal("no path in the svg")
	}
	for _, run := range strings.Split(body[start+3:], "M") {
		var x, y, width int
		if _, err := fmt.Sscanf(run, "%d %dh%d", &x, &y, &width); err != nil {
			continue
		}
		for offset := 0; offset < width; offset++ {
			drawn[[2]int{x + offset, y}] = true
		}
	}
	if len(drawn) == 0 {
		t.Fatal("the svg path drew nothing")
	}
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			// quietZone offsets the drawing, so compare in drawn coordinates.
			if code.Black(x, y) != drawn[[2]int{x + quietZone, y + quietZone}] {
				t.Fatalf("module (%d,%d) differs: encoder=%v svg=%v",
					x, y, code.Black(x, y), drawn[[2]int{x + quietZone, y + quietZone}])
			}
		}
	}
	// And the quiet zone really is quiet — a code drawn flush to the edge often
	// will not read at all.
	for _, edge := range [][2]int{{0, 0}, {quietZone - 1, quietZone - 1}, {code.Size + quietZone, code.Size + quietZone}} {
		if drawn[edge] {
			t.Fatalf("a module was drawn in the quiet zone at %v", edge)
		}
	}
}

// Every payload the round-trip test parses, BuildURL wrote. That is a closed
// loop: a convention both halves share wrongly passes it. workass-mobile hit the
// same shape from the other side — their stand-in encoder emitted a
// percent-escaped colon while the shipped one elides the default port, both
// parsed, and nothing failed while the scanner was exercised against a code no
// machine will ever draw.
//
// These are hand-composed: what a foreign writer, an older build, or a human
// retyping a code might hand us.
func TestParseAcceptsPayloadsThisPackageDidNotWrite(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    Join
	}{
		{
			// The riskiest case, because it must be RESTORED rather than read.
			name:    "an elided default port comes back",
			payload: "workass://join?h=192.168.0.13&k=" + testKey + "&n=Mac",
			want:    Join{Host: "192.168.0.13", Port: DefaultPort, Key: testKey, Name: "Mac"},
		},
		{
			name:    "a percent-escaped colon reads the same as a literal one",
			payload: "workass://join?h=192.168.1.50%3A18788&k=" + testKey,
			want:    Join{Host: "192.168.1.50", Port: 18788, Key: testKey},
		},
		{
			name:    "a literal colon",
			payload: "workass://join?h=192.168.1.50:18788&k=" + testKey,
			want:    Join{Host: "192.168.1.50", Port: 18788, Key: testKey},
		},
		{
			name:    "a key retyped by a human, upper case and dashed",
			payload: "workass://join?h=192.168.0.13&k=WF-DXAV-2WVJ-OK3K-J6HM-IR7Y-MWDJ5Y",
			want:    Join{Host: "192.168.0.13", Port: DefaultPort, Key: testKey},
		},
		{
			name:    "a name with accents survives percent-decoding",
			payload: "workass://join?h=192.168.0.13&k=" + testKey + "&n=Mac%20de%20Mart%C3%ADn",
			want:    Join{Host: "192.168.0.13", Port: DefaultPort, Key: testKey, Name: "Mac de Martín"},
		},
		{
			name:    "a hostname rather than an address",
			payload: "workass://join?h=builder.local:18788&k=" + testKey,
			want:    Join{Host: "builder.local", Port: 18788, Key: testKey},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			join, err := ParseURL(testCase.payload)
			if err != nil {
				t.Fatalf("parse %q: %v", testCase.payload, err)
			}
			if join != testCase.want {
				t.Fatalf("parsed %q\n got  %+v\n want %+v", testCase.payload, join, testCase.want)
			}
		})
	}
}
