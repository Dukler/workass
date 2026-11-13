package wire

import (
	"bytes"
	"compress/flate"
	"io"
	"strings"
	"testing"
)

// A segmented reply inflates, as a client inflates a compressed message, to
// exactly the bytes of the same reply sent plain.
func TestSegmentedReplyFrameInflatesToThePlainReply(t *testing.T) {
	big := strings.Repeat(`{"kind":"tool","title":"Read a file","status":"completed"},`, 3000)
	big = big[:len(big)-1]
	stored := strings.Repeat("x", 70000) // more than one stored block
	parts := []SegmentedPart{
		{Literal: []byte(`{"events":[`)},
		{Deflated: DeflateSegment([]byte(big)), Size: len(big)},
		{Literal: []byte(`],"pad":"` + stored + `","more":[`)},
		{Deflated: DeflateSegment([]byte(`1,2,3`)), Size: 5},
		{Literal: []byte(`]}`)},
	}
	result := SegmentedResult{Parts: parts}
	plain, err := result.PlainBytes()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"events":[` + big + `],"pad":"` + stored + `","more":[1,2,3]}`
	if string(plain) != want {
		t.Fatalf("plain bytes differ")
	}
	frame, replyBytes, err := encodeSegmentedReplyFrame(42, result)
	if err != nil {
		t.Fatal(err)
	}
	if frame[0] != 0xC1 {
		t.Fatalf("frame byte 0 = %#x, want FIN|RSV1|text", frame[0])
	}
	payload, ok := serverTextFramePayload(append([]byte{0x81}, frame[1:]...))
	if !ok {
		t.Fatal("not one complete frame")
	}
	reader := flate.NewReader(io.MultiReader(bytes.NewReader(payload), strings.NewReader(inflateMessageTail)))
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	plainFrame, plainBytes, err := encodeRawReplyFrame(42, RawResult(want), true)
	if err != nil {
		t.Fatal(err)
	}
	wantReply, _ := serverTextFramePayload(plainFrame)
	if !bytes.Equal(got, wantReply) {
		t.Fatalf("inflated reply differs from the plain reply:\n%.200s\n%.200s", got, wantReply)
	}
	if replyBytes != plainBytes {
		t.Fatalf("reply bytes %d, plain %d", replyBytes, plainBytes)
	}
	marshaled, err := result.MarshalJSON()
	if err != nil || string(marshaled) != want {
		t.Fatalf("MarshalJSON differs: %v", err)
	}
}

// A large reply compressed in parallel inflates to the same reply.
func TestParallelDeflateInflatesToThePlainReply(t *testing.T) {
	var plain bytes.Buffer
	plain.WriteString(`[`)
	for index := 0; plain.Len() < 5<<20; index++ {
		if index > 0 {
			plain.WriteByte(',')
		}
		plain.WriteString(`{"kind":"tool","title":"Read file ` + strings.Repeat("é", index%7) + `","n":` + string(rune('0'+index%10)) + `}`)
	}
	plain.WriteString(`]`)
	result := deflateParallel(plain.Bytes())
	if len(result.Parts) < 5 {
		t.Fatalf("%d parts", len(result.Parts))
	}
	frame, _, err := encodeSegmentedReplyFrame(7, result)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := serverTextFramePayload(append([]byte{0x81}, frame[1:]...))
	if !ok {
		t.Fatal("not one frame")
	}
	got, err := io.ReadAll(flate.NewReader(io.MultiReader(bytes.NewReader(payload), strings.NewReader(inflateMessageTail))))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"t":"reply","id":7,"result":` + plain.String() + `,"error":null}`
	if string(got) != want {
		t.Fatal("inflated reply differs")
	}
}
