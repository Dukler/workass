// Package fleetid is the fleet's name algebra: a global id for every entity,
// and the home daemon that owns it.
//
// Workass is already a small distributed system inside one machine — a chat
// actor per chat, one writer, a durable ledger — and it works because all of
// that shares one process. At the machine boundary the sharing stops, and
// until now the only name an entity had was the one its own daemon minted, so
// nothing outside that daemon could say which machine a chat, a subagent or a
// tracked process belonged to without being told separately.
//
// A global id says it in the string:
//
//	wa:<home-machine-id>/<type>/<local-id>
//	wa:m-7f3ad2/chat/app-chat-1792187130561
//	wa:m-7f3ad2/subagent/wa-subagent-1792187130561-8
//	wa:m-91be04/process/xw-6f12aa
//
// Two laws shape everything here (docs/specs/fleet-runtime.md §3):
//
//   - The local id is TODAY'S id, unchanged. A global id CONTAINS it; it never
//     replaces it. Every id this daemon ever minted is a valid local part
//     forever, so nothing is renamed, re-keyed or migrated to adopt this.
//   - The home is embedded, so routing is a string comparison: no directory
//     service, no lookup round trip, no table of which method may address what.
//     Router.Route is the whole of it — home == self takes the in-process path
//     the daemon already has.
//
// The legacy edge tag `M~<machine>~<local>`, which the renderer puts on ids as
// they cross into a client from another machine, is accepted on input as an
// alias and can be emitted again for clients that predate global ids.
//
// This package holds no state, starts nothing and talks to nothing. It is the
// vocabulary the rest of the runtime is written in.
package fleetid

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// Scheme prefixes every global id. It is three characters so a global id is
	// recognisable on sight in a log line beside the chat, tab and session ids
	// that look much the same.
	Scheme = "wa:"
	// LegacyPrefix is the machine tag the renderer has used at the socket
	// boundary since remote-plan E3 (desktop/renderer2/src/wire/machineIds.ts).
	// An id carrying it names the machine that minted it; an untagged id is the
	// local machine's, which is why Router treats "no home" as "my home".
	LegacyPrefix = "M~"
	// legacySeparator divides the machine from the local id in the legacy tag.
	// The FIRST one divides, so a local id may contain it and a home may not.
	legacySeparator = "~"
	// typeSeparator divides home, type and local id in a global id. The first
	// two divide; everything after the second belongs to the local id, so a
	// local id containing a slash still round-trips.
	typeSeparator = "/"
)

// Type is the entity kind carried in a global id. The set is closed: a kind
// that is not here has no global name yet, and inventing one silently would
// let two unrelated entities answer to the same string.
type Type string

const (
	// Chat is a durable chat actor: the owner of a reducer, a ledger and a
	// single-writer lock. Tab ids address the same actor pair and use this kind.
	Chat Type = "chat"
	// Turn is one prompt-to-settle run inside a chat.
	Turn Type = "turn"
	// Subagent is a delegated agent run owned by a parent turn.
	Subagent Type = "subagent"
	// Process is a tracked OS process: today's SpawnedWorkItem and external
	// work. Its home is the machine whose files and pid namespace it lives in.
	Process Type = "process"
	// Wait is a condition held at a home: today's scheduled message.
	Wait Type = "wait"
	// Fork is a fork point sealed from a chat's native session.
	Fork Type = "fork"
	// ForkRun is one run of a fork point, with its children.
	ForkRun Type = "forkrun"
	// Peer is another daemon, as named by the machine book.
	Peer Type = "peer"
)

// Types lists every entity kind that has a global name, in the spec's order.
// Callers that must handle "one of each" iterate this rather than repeating the
// list, so a kind added here cannot be forgotten by one of them.
func Types() []Type {
	return []Type{Chat, Turn, Subagent, Process, Wait, Fork, ForkRun, Peer}
}

// Valid reports whether t is one of the closed set of entity kinds.
func (t Type) Valid() bool {
	switch t {
	case Chat, Turn, Subagent, Process, Wait, Fork, ForkRun, Peer:
		return true
	}
	return false
}

// ErrNotGlobal reports a string that is not a global id. It is a sentinel so a
// caller can tell "this is some other id shape" from "this is a global id and
// it is malformed", which are different faults: the first is ordinary, the
// second is a bug or a tampered payload.
var ErrNotGlobal = errors.New("not a global fleet id")

// ID is one entity's global name.
type ID struct {
	// Home is the machine id of the daemon that owns this entity's write lock
	// and its log.
	Home string
	// Type is the entity kind.
	Type Type
	// Local is the id the home itself uses — today's id, byte for byte.
	Local string
}

// New builds a global id, rejecting the shapes that could not be parsed back.
func New(home string, kind Type, local string) (ID, error) {
	id := ID{Home: strings.TrimSpace(home), Type: kind, Local: strings.TrimSpace(local)}
	if err := id.validate(); err != nil {
		return ID{}, err
	}
	return id, nil
}

func (id ID) validate() error {
	switch {
	case id.Home == "":
		return errors.New("a global fleet id needs a home machine id")
	case strings.ContainsAny(id.Home, typeSeparator+legacySeparator):
		// A home holding either separator would make its own id ambiguous to
		// parse, in the global form or the legacy alias.
		return fmt.Errorf("home machine id %q may not contain %q or %q", id.Home, typeSeparator, legacySeparator)
	case strings.ContainsAny(id.Home, " \t\r\n"):
		return fmt.Errorf("home machine id %q may not contain whitespace", id.Home)
	case !id.Type.Valid():
		return fmt.Errorf("unknown fleet entity type %q", string(id.Type))
	case id.Local == "":
		return errors.New("a global fleet id needs a local id")
	}
	return nil
}

// String is the global id. The zero ID stringifies to "" rather than to a
// half-built id, so a forgotten check shows up as an empty field instead of as
// an id that addresses something unintended.
func (id ID) String() string {
	if id.validate() != nil {
		return ""
	}
	return Scheme + id.Home + typeSeparator + string(id.Type) + typeSeparator + id.Local
}

// Legacy is the same entity in the `M~<machine>~<local>` tag the renderer and
// the relay already speak. It is lossy on purpose — the type is dropped,
// because no client that predates global ids has anywhere to put it.
func (id ID) Legacy() string {
	if id.validate() != nil {
		return ""
	}
	return LegacyPrefix + id.Home + legacySeparator + id.Local
}

// Parse reads a global id. Everything after the second separator is the local
// id, so a local id that contains a slash survives the round trip.
func Parse(value string) (ID, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, Scheme) {
		return ID{}, fmt.Errorf("%w: %q", ErrNotGlobal, value)
	}
	parts := strings.SplitN(strings.TrimPrefix(value, Scheme), typeSeparator, 3)
	if len(parts) != 3 {
		return ID{}, fmt.Errorf("malformed global fleet id %q: want %shome/type/local", value, Scheme)
	}
	id := ID{Home: parts[0], Type: Type(parts[1]), Local: parts[2]}
	if err := id.validate(); err != nil {
		return ID{}, fmt.Errorf("malformed global fleet id %q: %w", value, err)
	}
	return id, nil
}

// IsGlobal reports whether value is in the global form. It says nothing about
// whether that form is well-made.
func IsGlobal(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), Scheme)
}

// IsLegacyTagged reports whether value carries the legacy machine tag.
func IsLegacyTagged(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), LegacyPrefix)
}

// IsHomed reports whether value is written in either home-bearing form. It is
// the loose test, for a field that is known to be an addressing id: a value
// that opens with the scheme but is malformed answers true here so the caller
// reports the malformed id rather than quietly treating it as a local one.
func IsHomed(value string) bool {
	return IsGlobal(value) || IsLegacyTagged(value)
}

// NamesHome reports whether value actually names a home — the legacy tag, or a
// global id that parses. It is the strict test, for a scan that reaches free
// text as well as ids: prose that merely opens with the scheme names no machine
// and must not be mistaken for an id that does.
func NamesHome(value string) bool {
	if IsLegacyTagged(value) {
		return true
	}
	_, err := Parse(value)
	return err == nil
}

// Split accepts any of the three shapes an addressing id can arrive in and
// reports the home and the local id:
//
//   - a global id            -> that id's home and local part
//   - a legacy `M~m-x~local` -> the tagged machine and the local part
//   - a bare local id        -> no home, the value unchanged
//
// kind, when not empty, is the entity kind the caller is addressing: a global
// id of a different kind is refused rather than resolved, because a subagent
// id passed where a chat id belongs would otherwise be looked up as a chat.
// The legacy form carries no kind, so it is taken at the caller's word.
func Split(value string, kind Type) (home, local string, homed bool, err error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return "", "", false, nil
	case IsGlobal(value):
		id, parseErr := Parse(value)
		if parseErr != nil {
			return "", "", false, parseErr
		}
		if kind != "" && id.Type != kind {
			return "", "", false, fmt.Errorf("%s names a %s, not a %s", value, string(id.Type), string(kind))
		}
		return id.Home, id.Local, true, nil
	case IsLegacyTagged(value):
		rest := strings.TrimPrefix(value, LegacyPrefix)
		cut := strings.Index(rest, legacySeparator)
		if cut <= 0 || cut == len(rest)-1 {
			// The wording is the one this edge has always reported, so a caller
			// that matched on it keeps matching.
			return "", "", false, errors.New("malformed machine-tagged chat id")
		}
		return rest[:cut], rest[cut+1:], true, nil
	default:
		return "", value, false, nil
	}
}

// Router answers the one question routing asks: is this entity mine?
//
// It holds nothing but this daemon's machine id. That is the whole mechanism —
// §2 of the spec: "the local API IS the fleet API with home == self". A call
// whose target is at home takes the in-process path the daemon already has,
// unchanged; a call whose target is elsewhere needs a link, which is P2.
type Router struct {
	self string
}

// NewRouter binds a router to this daemon's machine identity
// (internal/machineid). An empty self is allowed and means "identity not known
// yet": such a router refuses to claim anything as local rather than guessing,
// because claiming another machine's entity would address the wrong chat.
func NewRouter(self string) Router {
	return Router{self: strings.TrimSpace(self)}
}

// Self is this daemon's machine id.
func (r Router) Self() string { return r.self }

// Resolution is where one addressing id leads.
type Resolution struct {
	// Home is the machine that owns the entity.
	Home string
	// Local is the id its home knows it by — what the in-process path expects.
	Local string
	// Self is true when Home is this daemon, which is the only case P1 routes.
	Self bool
	// Homed records whether the caller named a home or left it implicit.
	Homed bool
}

// Route resolves one addressing id.
//
// An id with no home is this daemon's: that is migration step M0, where every
// entity that exists today is at home here without a byte of its stored state
// changing. An id naming this daemon resolves to exactly the same in-process
// path as the bare local id it contains, which is what makes a global id safe
// to hand back to a tool that only ever knew local ones.
func (r Router) Route(value string, kind Type) (Resolution, error) {
	home, local, homed, err := Split(value, kind)
	if err != nil {
		return Resolution{}, err
	}
	if local == "" {
		// An empty id addresses nothing, so it has no home to compare. Saying
		// so is better than reporting it as local, which would send a missing
		// id down the in-process path as if it had been supplied.
		return Resolution{}, nil
	}
	if !homed {
		return Resolution{Home: r.self, Local: local, Self: r.self != ""}, nil
	}
	return Resolution{Home: home, Local: local, Self: r.self != "" && home == r.self, Homed: true}, nil
}

// Home is the machine that owns value, with no home meaning this daemon.
func (r Router) Home(value string, kind Type) (string, error) {
	resolved, err := r.Route(value, kind)
	if err != nil {
		return "", err
	}
	return resolved.Home, nil
}

// ID stamps this daemon as the home of one of its own local ids. This is how
// an existing entity acquires a global name: the local id is copied, not
// rewritten, so the global id and the stored id stay the same string in the
// only place that matters.
func (r Router) ID(kind Type, local string) (ID, error) {
	if r.self == "" {
		return ID{}, errors.New("this daemon has no machine identity yet, so it cannot name its own entities globally")
	}
	return New(r.self, kind, local)
}

// Address is value in the shape today's local and relay paths understand.
//
// A self-homed id loses its home, because a local id has always travelled
// untagged here and every persisted draft, queue entry and archive filename
// matches the untagged form. Another home's id is emitted as the legacy tag,
// which is exactly the client that predates global ids the spec reserves it
// for: the renderer relay. An unhomed id is returned untouched.
func (r Router) Address(value string, kind Type) (string, error) {
	resolved, err := r.Route(value, kind)
	if err != nil {
		return "", err
	}
	switch {
	case resolved.Local == "":
		return strings.TrimSpace(value), nil
	case resolved.Self, !resolved.Homed:
		return resolved.Local, nil
	default:
		return LegacyPrefix + resolved.Home + legacySeparator + resolved.Local, nil
	}
}
