// Package voice turns a recorded utterance into text.
//
// Dictation, not streaming. whisper decodes a complete audio segment and has no
// notion of a partial hypothesis, so anything that looks like live subtitles is
// really the same audio decoded over and over through a sliding window — which
// is why such text visibly rewrites itself mid-sentence. We do not do that. The
// client records while the microphone is open, sends the whole utterance once,
// and gets back one final string. There is no provisional state to display, and
// therefore no wrong provisional text the user cannot edit.
//
// The text lands in the composer, editable. Audio is never a message: nothing is
// sent on the user's behalf, it is inserted for them to read, fix and send.
//
// Vocabulary biasing is the difference between usable and useless here. whisper
// hears "lambbridge.go" for lan_bridge.go and "usefleet" for useFleet; given the
// same identifiers as an initial prompt it returns both exactly, casing and all.
// Measured on this repo's own names — see TestPromptCarriesVocabulary.
package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// whisper wants 16 kHz mono signed 16-bit. The client resamples before it
	// sends, so the daemon never guesses a format.
	SampleRate = 16000
	Channels   = 1

	// A cap, not a target. Long enough for a paragraph of dictation, short
	// enough that a stuck microphone cannot hand us a gigabyte of silence.
	MaxAudioBytes = 32 << 20

	// Transcription is CPU-bound and finite: ~0.6s for 9s of speech on Metal,
	// several times that on the Windows laptop's CPU. If it has not finished in
	// two minutes something is wrong with the engine, not with the audio.
	transcribeTimeout = 2 * time.Minute
)

// ErrEngineMissing means whisper is not installed. It is a setup problem with a
// one-line fix, so it is worth telling apart from a transcription failure.
var ErrEngineMissing = errors.New("voice: no whisper engine found")

// ErrModelMissing means the engine is present but has no weights to run.
var ErrModelMissing = errors.New("voice: no whisper model found")

// Engine is a located whisper installation: a binary and a model file.
type Engine struct {
	Bin   string
	Model string
}

// Request is one utterance to transcribe.
type Request struct {
	// WAV is a complete RIFF/WAVE file, 16 kHz mono s16le.
	WAV []byte
	// Lang is an ISO code ("es", "en"). Empty means let whisper detect it.
	//
	// Detection is not free of consequence: a multilingual model asked to guess
	// on a sentence that switches languages mid-way sometimes translates instead
	// of transcribing. A caller that knows the language should say so.
	Lang string
	// Vocab are identifiers to bias decoding toward — file names, symbols, agent
	// names. Order is irrelevant; whisper sees them as prior context.
	Vocab []string
}

// Result is the finished transcription.
type Result struct {
	Text     string
	Model    string
	Lang     string
	Duration time.Duration
}

// Locate finds a usable engine, or says precisely which half is missing.
//
// Env overrides come first so an operator can pin an engine without touching
// PATH — the Windows laptop has no package manager to install one with, and its
// binaries arrive by being copied into place.
func Locate() (Engine, error) {
	bin := strings.TrimSpace(os.Getenv("WORKASS_WHISPER_BIN"))
	if bin == "" {
		for _, candidate := range []string{"whisper-cli", "whisper-cpp", "main"} {
			if found, err := exec.LookPath(candidate); err == nil {
				bin = found
				break
			}
		}
	}
	if bin == "" {
		for _, candidate := range []string{
			"/opt/homebrew/bin/whisper-cli",
			"/usr/local/bin/whisper-cli",
		} {
			if isFile(candidate) {
				bin = candidate
				break
			}
		}
	}
	if bin == "" {
		return Engine{}, ErrEngineMissing
	}

	model := strings.TrimSpace(os.Getenv("WORKASS_WHISPER_MODEL"))
	if model == "" {
		model = findModel()
	}
	if model == "" {
		return Engine{Bin: bin}, ErrModelMissing
	}
	return Engine{Bin: bin, Model: model}, nil
}

// findModel prefers the larger multilingual weights. The .en models are faster
// and better per byte, and useless to anyone who mixes two languages in one
// sentence — which is the actual usage here, so they are not candidates.
func findModel() string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, ".local", "share", "whisper-models"),
			filepath.Join(home, ".cache", "whisper"),
		)
	}
	roots = append(roots, "/opt/homebrew/share/whisper-cpp", "/usr/local/share/whisper-cpp")

	// Best first: quality matters more than speed at these sizes, because the
	// wait happens after the user stops talking rather than during.
	names := []string{
		"ggml-large-v3-turbo.bin", "ggml-large-v3.bin",
		"ggml-medium.bin", "ggml-small.bin", "ggml-base.bin", "ggml-tiny.bin",
	}
	for _, root := range roots {
		for _, name := range names {
			path := filepath.Join(root, name)
			if isFile(path) {
				return path
			}
		}
	}
	return ""
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Prompt renders the vocabulary as an initial prompt.
//
// whisper treats this as text that preceded the audio, so it reads as context
// rather than as a command — a bare list of identifiers works, and phrasing it
// as an instruction does not, because there is no instruction-follower here.
func Prompt(vocab []string) string {
	seen := make(map[string]bool, len(vocab))
	kept := make([]string, 0, len(vocab))
	for _, term := range vocab {
		term = strings.TrimSpace(term)
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		kept = append(kept, term)
		// n_text_ctx/2 caps the prompt; well before that a long list starts
		// crowding out the audio's own context and accuracy drops.
		if len(kept) >= 96 {
			break
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, ", ") + "."
}

// Transcribe runs one utterance through the engine and returns the final text.
func (e Engine) Transcribe(ctx context.Context, req Request) (Result, error) {
	if e.Bin == "" {
		return Result{}, ErrEngineMissing
	}
	if e.Model == "" {
		return Result{}, ErrModelMissing
	}
	if len(req.WAV) == 0 {
		return Result{}, errors.New("voice: empty audio")
	}
	if len(req.WAV) > MaxAudioBytes {
		return Result{}, fmt.Errorf("voice: audio is %d bytes, over the %d cap", len(req.WAV), MaxAudioBytes)
	}
	if !bytes.HasPrefix(req.WAV, []byte("RIFF")) {
		return Result{}, errors.New("voice: audio is not a RIFF/WAVE file")
	}

	dir, err := os.MkdirTemp("", "workass-voice-")
	if err != nil {
		return Result{}, fmt.Errorf("voice: temp dir: %w", err)
	}
	// The recording is the user's speech. It exists for as long as one decode
	// takes and never outlives the call, on any path out of it.
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "utterance.wav")
	if err := os.WriteFile(path, req.WAV, 0o600); err != nil {
		return Result{}, fmt.Errorf("voice: write audio: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()

	args := []string{
		"-m", e.Model,
		"-f", path,
		"-nt", // no timestamps: we want a sentence, not a subtitle track
		"-np", // no progress chatter on stderr
	}
	if lang := strings.TrimSpace(req.Lang); lang != "" {
		args = append(args, "-l", lang)
	}
	if prompt := Prompt(req.Vocab); prompt != "" {
		args = append(args, "--prompt", prompt)
	}

	started := time.Now()
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("voice: transcription timed out after %s", transcribeTimeout)
		}
		return Result{}, fmt.Errorf("voice: %w: %s", err, tail(stderr.String()))
	}

	return Result{
		Text:     Clean(stdout.String()),
		Model:    filepath.Base(e.Model),
		Lang:     req.Lang,
		Duration: time.Since(started),
	}, nil
}

// Clean turns whisper's stdout into something insertable.
//
// It emits a leading space on every segment and marks non-speech with bracketed
// tags — [BLANK_AUDIO], [Music], [ Silence ] — which are descriptions of the
// recording, not words anyone said. Dropping them is what makes an accidental
// tap on the microphone insert nothing instead of the word "blank audio".
func Clean(raw string) string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	const max = 400
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// WAV wraps raw 16 kHz mono s16le samples in a RIFF header.
//
// Kept here so a Go caller (tests, probes, a future non-browser client) can make
// an utterance without a browser's help; the renderer builds the same header in
// JavaScript because it holds the samples.
func WAV(pcm []byte) []byte {
	var buf bytes.Buffer
	blockAlign := Channels * 2
	byteRate := SampleRate * blockAlign

	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(Channels))
	binary.Write(&buf, binary.LittleEndian, uint32(SampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}
