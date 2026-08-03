package machinebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// HealthPath is the identity document every daemon serves.
const HealthPath = "/workass/health"

// maxCardBytes bounds what a probe will read. The address being probed is
// whatever a human typed or whatever announced itself on the network, so it may
// be any HTTP server at all — including one happy to stream forever.
const maxCardBytes = 64 << 10

// Card is a daemon's answer to "who are you?", parsed.
//
// A caller across the network is told less than a local one: identity and wire
// version, never the provider inventory or the OS. So every field beyond the id
// is optional by construction, and this struct is deliberately the shape of the
// *public* document — the book reads a remote machine and a local one with the
// same code.
type Card struct {
	App         string   `json:"app"`
	Name        string   `json:"name"`
	Label       string   `json:"displayName"`
	MachineID   string   `json:"machineId"`
	Version     string   `json:"version"`
	WireVersion int      `json:"wireVersion"`
	Secure      bool     `json:"secure"`
	FleetIDs    []string `json:"fleetIds"`
}

// DisplayName is the label to show, preferring the one the machine chose for
// itself over the hostname fallback.
func (c Card) DisplayName() string {
	if label := strings.TrimSpace(c.Label); label != "" {
		return label
	}
	return strings.TrimSpace(c.Name)
}

// ErrNotWorkass reports an address that answered but is not a daemon. It is its
// own error because the two failures need different words: nothing listening is
// a network problem, and a web server answering is a wrong-address problem.
var ErrNotWorkass = errors.New("not a workass daemon")

// ErrNoIdentity reports a daemon too old to have a machine id. Such a daemon
// cannot be tracked — the book is keyed by id precisely so that a machine which
// changes address stays one machine.
var ErrNoIdentity = errors.New("daemon has no machine id")

// probe asks one address who it is.
//
// Plaintext for now, which is the whole reason `secure` rides on the card: E5
// gives a daemon its own certificate and this becomes https-first, and until
// then the flag is what lets a client say out loud that a machine is in the
// clear rather than quietly assuming it is not.
func (b *Book) probe(ctx context.Context, address string) (Card, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+HealthPath, nil)
	if err != nil {
		return Card{}, err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return Card{}, fmt.Errorf("%s %s", address, whyItFailed(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Card{}, fmt.Errorf("%s answered %d: %w", address, response.StatusCode, ErrNotWorkass)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCardBytes))
	if err != nil {
		return Card{}, fmt.Errorf("%s answered but the reply broke off: %w", address, err)
	}
	var card Card
	if err := json.Unmarshal(body, &card); err != nil {
		return Card{}, fmt.Errorf("%s: %w", address, ErrNotWorkass)
	}
	if strings.TrimSpace(card.App) != "workass" {
		return Card{}, fmt.Errorf("%s: %w", address, ErrNotWorkass)
	}
	card.MachineID = strings.TrimSpace(card.MachineID)
	if card.MachineID == "" {
		return Card{}, fmt.Errorf("%s: %w — update it", address, ErrNoIdentity)
	}
	return card, nil
}

// whyItFailed says what went wrong in words. This string is the reason shown
// beside an unreachable machine, and Go's transport errors carry the whole URL
// and dial chain — true, and useless to read. The distinction that matters to
// someone looking at the list is which of three things happened: nobody is
// listening, nothing came back, or that name means nothing here.
func whyItFailed(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return "could not be found"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused the connection"
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return "did not answer in time"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "is not reachable from here"
	default:
		return "did not answer: " + err.Error()
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
