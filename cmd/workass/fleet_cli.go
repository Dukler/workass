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
	"strconv"
	"strings"

	"workass/internal/fleet"
	"workass/internal/fleetqr"
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
