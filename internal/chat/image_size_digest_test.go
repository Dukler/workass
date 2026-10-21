package chat

import (
	"testing"

	"workass/internal/provider"
)

// Attachment pixel sizes are display metadata. A seed, delta or import digest
// an older build recorded over a ledger range must still match when a newer
// build, whose rows carry sizes, replays the same range.
func TestContextDigestsIgnoreAttachmentPixelSize(t *testing.T) {
	ledger := func(width, height int) State {
		state, _ := NewState("chat")
		state.Ledger = []LedgerEvent{
			{Sequence: 1, EventID: "e1", MessageID: "m1", Role: "user", Text: "look", Status: "done", OperationID: "op1",
				Attachments: []provider.Attachment{{ID: "image-0123456789abcdef", Name: "shot.png", MIMEType: "image/png",
					Digest: "0123456789abcdef", Size: 12, Ref: "ref", Width: width, Height: height}}},
			{Sequence: 2, EventID: "e2", MessageID: "m2", Role: "assistant", Text: "seen", Status: "done", OperationID: "op1"},
		}
		return state
	}
	unsized, sized := ledger(0, 0), ledger(1440, 900)
	before, err := buildInitialSeedBatch(unsized, 0, unsized.LedgerHead())
	if err != nil {
		t.Fatal(err)
	}
	after, err := buildInitialSeedBatch(sized, 0, sized.LedgerHead())
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest == "" || before.Digest != after.Digest {
		t.Fatalf("seed digest changed with the size: %s vs %s", before.Digest, after.Digest)
	}
	if got := after.Messages[0].Attachments[0]; got.Width != 0 || got.Height != 0 || got.Digest != "0123456789abcdef" {
		t.Fatalf("context attachment = %#v", got)
	}
	if after.Messages[1].Attachments != nil {
		t.Fatal("a row without attachments must keep a nil list")
	}
	if sized.Ledger[0].Attachments[0].Width != 1440 {
		t.Fatal("building the seed cleared the ledger's size")
	}
	capabilities := provider.ContextCapabilities{MaxImportEvents: 10, MaxImportBytes: 1 << 20}
	importBefore, _, _, err := buildContextBatch(unsized, 0, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	importAfter, _, _, err := buildContextBatch(sized, 0, capabilities)
	if err != nil || importBefore.Digest != importAfter.Digest {
		t.Fatalf("import digest changed with the size: %s vs %s (%v)", importBefore.Digest, importAfter.Digest, err)
	}
}
