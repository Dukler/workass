package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"workass/internal/fleet"
	"workass/internal/fleetclient"
	"workass/internal/fleetqr"
	"workass/internal/lease"
)

// dataRootEnvVar is set by the launchd/systemd unit and by every profile in
// config/environments, so a shell that has sourced one already knows where this
// machine's daemon keeps its state.
const dataRootEnvVar = "WORKASS_DATA_ROOT"

// defaultFleetStateDir resolves where the RUNNING daemon keeps its state, so a
// fleet command typed from a random directory acts on this machine instead of
// inventing a second fleet beside the shell's cwd.
//
// The old default was the relative path "state", which meant `workass fleet qr`
// in ~/code produced a code for ~/code/state — a fleet of one that no daemon has
// ever heard of. It scanned perfectly and enrolled nowhere.
func defaultFleetStateDir() string {
	if root := strings.TrimSpace(os.Getenv(dataRootEnvVar)); root != "" {
		return filepath.Join(root, "state")
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			candidate := filepath.Join(home, "Library", "Application Support", "Workass", "state")
			// Only when a key is actually there. Guessing a path that happens not
			// to exist would just move the invented-fleet problem somewhere less
			// visible than the cwd.
			if _, err := os.Stat(filepath.Join(candidate, fleet.FileName)); err == nil {
				return candidate
			}
		}
	}
	return "state"
}

// hostname is the cosmetic label on a join code — it tells the human holding
// the phone which machine they are pointing at. The phone replaces it with
// whatever /workass/health reports, so a failure here costs nothing.
func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(name), ".local")
}

// runFleetCommand is the whole human side of trust: one key, shown on the
// machine that has it and pasted into the machines that do not.
//
// The key is never taken from argv. A command line is visible to every process
// on the box through ps and lands in shell history; a paste on stdin does
// neither, which is why `join` reads there and nowhere else.
func runFleetCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("fleet", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDirFlag := flags.String("state-dir", defaultFleetStateDir(), "daemon state directory")
	label := flags.String("label", "", "note recorded with the key, for your own memory")
	flags.Usage = func() {
		fmt.Fprint(stderr, `usage: workass fleet <command> [flags]

  key       print this machine's fleet key, minting one if it has none
  join      read a fleet key on stdin and join that fleet
  list      show the fleets this machine belongs to, without their secrets
  rotate    mint a replacement key, keeping the old one until you drop it
  forget    stop accepting enrolments under a key id
  qr        draw a scannable join code in the terminal, for a machine with
            no screen: workass fleet qr [host[:port]]
  links     show the peer daemons linked to this machine, and the links it
            dialled out itself. Read-only: revoking one is fleet:unlink,
            which also fences the live socket.

`)
		flags.PrintDefaults()
	}
	// The command comes first, then flags. Go's flag package stops parsing at the
	// first non-flag word, so `fleet key --state-dir X` would otherwise ignore
	// --state-dir without a word and mint a key into the wrong machine's state.
	if len(args) == 0 {
		flags.Usage()
		return fmt.Errorf("a command is required")
	}
	command := strings.TrimSpace(args[0])
	if command == "" || strings.HasPrefix(command, "-") {
		flags.Usage()
		return fmt.Errorf("the command comes first: workass fleet <command> [flags]")
	}
	// Flags and positionals interleave freely after the command: flag.Parse stops
	// at each non-flag word, so alternate between parsing and taking one. Without
	// this, `fleet forget <id> --state-dir X` acts on the wrong machine's state.
	positional := make([]string, 0, 2)
	for rest := args[1:]; len(rest) > 0; {
		if err := flags.Parse(rest); err != nil {
			return err
		}
		rest = flags.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}

	stateDir := *stateDirFlag
	if !filepath.IsAbs(stateDir) {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		stateDir = filepath.Join(cwd, stateDir)
	}
	store, err := fleet.Open(stateDir)
	if err != nil {
		return err
	}

	switch command {
	case "key":
		key, minted, err := store.EnsureKey()
		if err != nil {
			return err
		}
		if minted {
			fmt.Fprintf(stdout, "Minted a new fleet key for %s\n\n", stateDir)
		}
		fmt.Fprintf(stdout, "%s\n\n", key.Secret)
		fmt.Fprintf(stdout, "key id %s · owner %s\n", key.KeyID, key.Owner)
		fmt.Fprint(stdout, "Paste it into every other machine with `workass fleet join`, and into\n"+
			"each client once. Treat it like a WiFi password: anything holding it can\n"+
			"drive every machine in this fleet.\n")
		return nil

	case "join":
		secret, err := readSecret(stdin, stdout, stderr)
		if err != nil {
			return err
		}
		key, err := store.Join(secret, fleet.LocalOwner, *label)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "joined fleet %s\n", key.KeyID)
		fmt.Fprint(stdout, "Clients holding this key can now enrol here on their own.\n")
		return nil

	case "links":
		// Read-only on purpose. The running daemon holds devices.json in memory
		// and rewrites it on its next change, so a revocation typed here would be
		// silently undone — exactly the kind of half-true state this design
		// exists to remove. Revoking a link is `fleet:unlink` on the wire, which
		// fences the socket in the same breath.
		return printFleetLinks(stateDir, stdout)

	case "list":
		keys := store.Keys()
		if len(keys) == 0 {
			fmt.Fprintf(stdout, "no fleet key in %s — run `workass fleet key` to mint one\n", stateDir)
			return nil
		}
		for _, key := range keys {
			line := fmt.Sprintf("%s  owner=%s  created=%s", key.KeyID, key.Owner, key.CreatedAt)
			if key.Label != "" {
				line += "  " + key.Label
			}
			fmt.Fprintln(stdout, line)
		}
		return nil

	case "rotate":
		// Both keys are live until you drop the old one. Rotating by replacement
		// would lock out every machine that had not been updated yet, and there
		// is no daemon-to-daemon channel to update them over — by design, trust
		// runs client-to-daemon only.
		secret, err := fleet.NewSecret()
		if err != nil {
			return err
		}
		key, err := store.Join(secret, fleet.LocalOwner, strings.TrimSpace(*label+" rotation"))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n\n", key.Secret)
		fmt.Fprintf(stdout, "key id %s\n\n", key.KeyID)
		fmt.Fprint(stdout, "Both keys work right now. Join this one everywhere, then run\n"+
			"`workass fleet forget <old key id>` on every machine to close the old one.\n"+
			"Devices already enrolled keep working either way: their tokens do not\n"+
			"depend on the key that admitted them.\n")
		return nil

	case "forget":
		keyID := ""
		if len(positional) > 0 {
			keyID = strings.TrimSpace(positional[0])
		}
		if keyID == "" {
			return fmt.Errorf("forget needs a key id; run `workass fleet list`")
		}
		dropped, err := store.Forget(keyID)
		if err != nil {
			return err
		}
		if !dropped {
			return fmt.Errorf("no key %s here", keyID)
		}
		fmt.Fprintf(stdout, "forgot %s; no new device can enrol under it\n", keyID)
		fmt.Fprint(stdout, "Devices already enrolled are unaffected — revoke those individually.\n")
		return nil

	case "qr":
		// The reason this exists: a headless machine has no sheet to draw on, and
		// typing 26 base32 characters into a phone is the worst part of enrolling
		// one. Over SSH the terminal IS the screen.
		//
		// It reads the key and never mints one. `qr` reads like `list`, so a mint
		// hidden inside it is a credential created by a command that looks like a
		// question — and the code it then draws belongs to a fleet no daemon is
		// running. Refusing names the path, which is something the user can act on;
		// minting produces a QR that scans perfectly and enrols nowhere.
		existing := store.Keys()
		if len(existing) == 0 {
			return fmt.Errorf("no fleet key in %s\n"+
				"Run `workass fleet key` there to mint one, or point at the daemon's\n"+
				"state directory with --state-dir.", stateDir)
		}
		key := existing[0]
		host := ""
		if len(positional) > 0 {
			host = strings.TrimSpace(positional[0])
		}
		if host == "" {
			reachable := fleetqr.ReachableHosts()
			if len(reachable) == 0 {
				return fmt.Errorf("no reachable address found; pass one: workass fleet qr <host[:port]>")
			}
			host = reachable[0]
			if len(reachable) > 1 {
				fmt.Fprintf(stderr, "using %s; this machine also answers on %s\n",
					host, strings.Join(reachable[1:], ", "))
			}
		}
		join := fleetqr.Join{Host: host, Key: key.Secret, Name: hostname()}
		if splitHost, splitPort, splitErr := net.SplitHostPort(host); splitErr == nil {
			port, portErr := strconv.Atoi(splitPort)
			if portErr != nil {
				return fmt.Errorf("%q has no usable port", host)
			}
			join.Host, join.Port = splitHost, port
		}
		payload, err := fleetqr.BuildURL(join)
		if err != nil {
			return err
		}
		block, err := fleetqr.Terminal(payload)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\n%s\n%s\n\n", block, payload)
		fmt.Fprintf(stdout, "Scan it with the Workass app on the phone.\n")
		// The state directory is named because a code from the wrong one is
		// indistinguishable from a correct one until a phone fails to enrol.
		fmt.Fprintf(stdout, "key id %s · owner %s · from %s\n", key.KeyID, key.Owner, stateDir)
		fmt.Fprint(stdout, "The code IS the key: anyone who photographs this screen joins the fleet.\n")
		return nil
	}

	flags.Usage()
	return fmt.Errorf("unknown fleet command: %s", command)
}

// readSecret takes the key from stdin. It prompts only when stdin is a terminal,
// so `workass fleet join < key.txt` and a piped paste both stay clean.
func readSecret(stdin io.Reader, stdout, stderr io.Writer) (string, error) {
	if file, ok := stdin.(*os.File); ok {
		if info, err := file.Stat(); err == nil && (info.Mode()&os.ModeCharDevice) != 0 {
			fmt.Fprint(stderr, "paste the fleet key, then press enter: ")
		}
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		if err == io.EOF {
			return "", fmt.Errorf("no fleet key on stdin")
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// fleetLinkRow is ONE pair, with both of its halves and the verdict that can
// only be read from the two of them together.
//
// It exists because this output was itself the B8 bug report. A link is one
// duplex connection per pair (§4) and either side may have dialled it, but the
// two records are written by different events — an inbound enrolment or beat,
// and an outbound dial — and they used to print in two separate blocks with no
// verdict in either. So a `lastUse` coalesced to five minutes (beatRecordInterval)
// read as a dead link, while the inbound half twelve lines further down carried
// a timestamp from thirty seconds ago and said the pair was fine. A format that
// makes a human cross-reference two blocks to answer "is this up?" will be read
// wrongly, and was.
type fleetLinkRow struct {
	peer     string
	inbound  *lease.Device
	outbound *fleetclient.Peer
	cut      *lease.BlockedPeer
}

// label is the name a human recognises. The outbound half holds the machine
// book's display name ("duklerver"); the inbound half holds the DEVICE name a
// dialling daemon states for itself, which is "workass daemon <host> (<machine
// id>)" and puts the machine id on the line twice. So the short one wins where
// there is a choice, and the long one is better than nothing.
func (row fleetLinkRow) label() string {
	if row.outbound != nil && strings.TrimSpace(row.outbound.Name) != "" {
		return strings.TrimSpace(row.outbound.Name)
	}
	if row.inbound != nil && strings.TrimSpace(row.inbound.Name) != "" {
		return strings.TrimSpace(row.inbound.Name)
	}
	if row.cut != nil {
		return strings.TrimSpace(row.cut.Name)
	}
	return ""
}

// fleetLinkVerdict is the headline: up, down, or cut, and how long the pair has
// been silent.
//
// The evidence is whichever half was seen most recently, because a pair has one
// link and either end may be the one keeping the record of it. The threshold is
// the same ten minutes §16.3a gives home_down (fleetHomeDownThreshold), so what
// this prints and what a watcher synthesizes cannot disagree — a CLI that calls
// a pair up while the runtime is emitting home_down for it would be worse than
// no verdict at all.
func (row fleetLinkRow) verdict(now time.Time) string {
	if row.cut != nil {
		return "CUT"
	}
	seen, ok := row.lastSeen()
	if !ok {
		// Paired but never seen since: this daemon restarted and nothing has
		// beaten yet, or the record predates any beat. Unknown is the honest
		// answer and is not the same as down.
		return "UNKNOWN  (no beat recorded yet)"
	}
	silent := now.Sub(seen)
	if silent < 0 {
		silent = 0
	}
	if silent < fleetHomeDownThreshold {
		return fmt.Sprintf("UP  (silent %s)", silent.Round(time.Second))
	}
	return fmt.Sprintf("DOWN  (silent %s, past the %s threshold)",
		silent.Round(time.Second), fleetHomeDownThreshold)
}

// lastSeen is the freshest evidence either half holds for this pair.
func (row fleetLinkRow) lastSeen() (time.Time, bool) {
	best, ok := time.Time{}, false
	consider := func(stamp string) {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(stamp))
		if err != nil {
			return
		}
		if !ok || at.After(best) {
			best, ok = at, true
		}
	}
	if row.inbound != nil {
		consider(row.inbound.LastSeen)
	}
	if row.outbound != nil {
		consider(row.outbound.LastUseAt)
	}
	return best, ok
}

// printFleetLinks shows this machine's mesh one pair at a time: the verdict
// first, then the inbound half (the device store's view) and the outbound half
// (the peer store's) indented as its evidence.
//
// Every pair prints both halves even when one of them is missing, because a
// machine that appears on one side and not the other is itself the finding —
// that absence is exactly what B8 was.
func printFleetLinks(stateDir string, stdout io.Writer) error {
	leases, err := lease.NewManager(lease.Options{StateDir: stateDir})
	if err != nil {
		return err
	}
	peers, err := fleetclient.Open(stateDir)
	if err != nil {
		return err
	}
	inbound := leases.PeerLinks()
	outbound := peers.Redacted()
	blocked := leases.BlockedPeers()

	rows := map[string]*fleetLinkRow{}
	order := []string{}
	at := func(peerID string) *fleetLinkRow {
		peerID = strings.TrimSpace(peerID)
		row, seen := rows[peerID]
		if !seen {
			row = &fleetLinkRow{peer: peerID}
			rows[peerID] = row
			order = append(order, peerID)
		}
		return row
	}
	for i := range inbound {
		at(inbound[i].Peer).inbound = &inbound[i]
	}
	for i := range outbound {
		at(outbound[i].MachineID).outbound = &outbound[i]
	}
	for i := range blocked {
		at(blocked[i].MachineID).cut = &blocked[i]
	}
	sort.Strings(order)

	// The two sentences a machine with an empty mesh needs, kept verbatim: they
	// are what an operator greps for, and "nobody has linked to me" and "I have
	// dialled nobody" are different facts about different halves.
	if len(inbound) == 0 {
		fmt.Fprintf(stdout, "no peer daemon has linked to this machine\n")
	}
	if len(outbound) == 0 {
		fmt.Fprintf(stdout, "this machine has dialled no peer\n")
	}
	now := time.Now().UTC()
	for _, peerID := range order {
		row := rows[peerID]
		fmt.Fprintf(stdout, "pair %s  name=%q  %s\n", row.peer, row.label(), row.verdict(now))
		if device := row.inbound; device != nil {
			grant := strings.TrimSpace(device.Grant)
			if grant == "" {
				grant = "full"
			}
			fmt.Fprintf(stdout, "     in   linked  key=%s  grant=%s  lastSeen=%s\n",
				device.KeyID, grant, firstNonEmptyCLI(device.LastSeen, "never"))
		} else {
			fmt.Fprintf(stdout, "     in   this peer has not linked in\n")
		}
		if peer := row.outbound; peer != nil {
			state := "paired"
			switch {
			case peer.RevokedByPeer:
				state = "revoked by that peer"
			case !peer.Linked():
				state = "known, not linked"
			}
			// The window is named on the line that carries the timestamp. An
			// operator who knows lastUse is written at most every five minutes
			// cannot misread a five-minute-old one as silence, which is the
			// misreading that opened B8.
			fmt.Fprintf(stdout, "     out  %s  lastUse=%s  (recorded at most every %s)\n",
				state, firstNonEmptyCLI(peer.LastUseAt, "never"), beatRecordWindow)
			for _, route := range peer.Routes {
				note := ""
				switch {
				case route.Refused:
					note = "  REFUSED: " + route.RefusedReason
				case route.CooldownUntil != "":
					note = "  cooling until " + route.CooldownUntil
				}
				fmt.Fprintf(stdout, "          %s  %s  %s  lastSuccess=%s%s\n",
					route.Kind, route.Address, route.Source, firstNonEmptyCLI(route.LastSuccessAt, "never"), note)
			}
		} else {
			// Not cosmetic: with no peer record this daemon has no address for
			// the machine and cannot dial it, which is the whole of B8 in one
			// line. A pair can still be UP on the inbound half while this says so.
			fmt.Fprintf(stdout, "     out  not dialled  (no peer record here, so this daemon cannot dial it)\n")
		}
		if cut := row.cut; cut != nil {
			fmt.Fprintf(stdout, "     cut  revoked=%s  (it cannot link again until it is approved)\n", cut.RevokedAt)
		}
	}
	return nil
}

// beatRecordWindow is how often a live link's heartbeat is written down, named
// here only so the output can say so. The number itself belongs to
// internal/fleetclient, which is the one place that decides it.
var beatRecordWindow = time.Duration(fleetclient.BeatRecordInterval())

func firstNonEmptyCLI(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
