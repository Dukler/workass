package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanDropsNonSpeechTags(t *testing.T) {
	// whisper describes a silent recording instead of returning nothing. An
	// accidental tap must insert an empty string, never the words "blank audio".
	for _, raw := range []string{"[BLANK_AUDIO]", " [Music] ", "[ Silence ]", "  \n \n"} {
		if got := Clean(raw); got != "" {
			t.Errorf("Clean(%q) = %q, want empty", raw, got)
		}
	}
}

func TestCleanJoinsSegments(t *testing.T) {
	raw := "\n Revisá el reducer \n de la sidebar.  \n"
	want := "Revisá el reducer de la sidebar."
	if got := Clean(raw); got != want {
		t.Errorf("Clean() = %q, want %q", got, want)
	}
}

func TestPromptDedupesAndCaps(t *testing.T) {
	if got := Prompt(nil); got != "" {
		t.Errorf("Prompt(nil) = %q, want empty", got)
	}
	if got := Prompt([]string{" ", ""}); got != "" {
		t.Errorf("Prompt(blank) = %q, want empty", got)
	}
	got := Prompt([]string{"useFleet", "useFleet", " lan_bridge.go "})
	if got != "useFleet, lan_bridge.go." {
		t.Errorf("Prompt() = %q", got)
	}

	many := make([]string, 200)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if n := strings.Count(Prompt(many), ",") + 1; n > 96 {
		t.Errorf("Prompt kept %d terms, want <= 96", n)
	}
}

func TestWAVHeaderIsWellFormed(t *testing.T) {
	pcm := make([]byte, 320) // 10ms at 16kHz mono s16
	w := WAV(pcm)

	if !bytes.HasPrefix(w, []byte("RIFF")) || !bytes.Contains(w, []byte("WAVEfmt ")) {
		t.Fatalf("not a RIFF/WAVE file")
	}
	if len(w) != 44+len(pcm) {
		t.Errorf("len = %d, want %d", len(w), 44+len(pcm))
	}
	if rate := binary.LittleEndian.Uint32(w[24:28]); rate != SampleRate {
		t.Errorf("sample rate = %d, want %d", rate, SampleRate)
	}
	if ch := binary.LittleEndian.Uint16(w[22:24]); ch != Channels {
		t.Errorf("channels = %d, want %d", ch, Channels)
	}
	if n := binary.LittleEndian.Uint32(w[40:44]); int(n) != len(pcm) {
		t.Errorf("data size = %d, want %d", n, len(pcm))
	}
}

func TestTranscribeRejectsBadInput(t *testing.T) {
	e := Engine{Bin: "whisper-cli", Model: "model.bin"}
	if _, err := e.Transcribe(context.Background(), Request{}); err == nil {
		t.Error("empty audio: want error")
	}
	if _, err := e.Transcribe(context.Background(), Request{WAV: []byte("not a wav")}); err == nil {
		t.Error("non-RIFF audio: want error")
	}
	if _, err := (Engine{}).Transcribe(context.Background(), Request{WAV: []byte("RIFF")}); err != ErrEngineMissing {
		t.Errorf("missing bin: got %v, want ErrEngineMissing", err)
	}
}

// TestPromptCarriesVocabulary is the claim the package doc makes, checked
// against a real decode rather than asserted.
//
// Without the vocabulary whisper returns "lambbridge.go" and "usefleet"; with
// it, both come back exactly, casing included. Needs the engine, a model and
// macOS `say` to synthesise the utterance — skips cleanly without them.
func TestPromptCarriesVocabulary(t *testing.T) {
	engine, err := Locate()
	if err != nil {
		t.Skipf("no whisper engine: %v", err)
	}
	say, err := exec.LookPath("say")
	if err != nil {
		t.Skip("no `say` to synthesise an utterance")
	}

	dir := t.TempDir()
	wavPath := filepath.Join(dir, "utt.wav")
	const utterance = "Mirá el archivo lan_bridge punto go y el reducer de useFleet."
	cmd := exec.Command(say, "-v", "Paulina", "-o", wavPath, "--data-format=LEI16@16000", utterance)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("say failed (no Spanish voice installed?): %v: %s", err, out)
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatalf("read synthesised audio: %v", err)
	}

	res, err := engine.Transcribe(context.Background(), Request{
		WAV:   wav,
		Lang:  "es",
		Vocab: []string{"lan_bridge.go", "useFleet", "httpserve", "reducer", "sidebar"},
	})
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}

	for _, want := range []string{"lan_bridge.go", "useFleet"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("transcript %q is missing the biased identifier %q", res.Text, want)
		}
	}
	t.Logf("model=%s in %s: %s", res.Model, res.Duration.Round(1e6), res.Text)
}
