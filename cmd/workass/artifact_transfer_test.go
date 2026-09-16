package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"workass/internal/artifacthost"
	"workass/internal/lease"
	"workass/internal/wire"
)

func TestArtifactTransferWirePairedRoundTripAndUnpairedReject(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "wire.pdf"), []byte("wire artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := artifacthost.New(state, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := reg.Register(artifacthost.RegisterOptions{BaseDir: workspace, SourcePath: "wire.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	{
		lm, err := lease.NewManager(lease.Options{StateDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		hub := wire.NewHub(wire.Options{TrustLocalhost: true, Lease: lm})
		registerArtifactTransferHandlers(hub, reg)
		srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		defer srv.Close()
		conn := artifactWireDial(t, srv.URL)
		defer conn.Close()
		result := artifactWireInvoke(t, conn, 1, "artifact:open", map[string]any{"path": hosted.URLPath, "method": "GET"})
		var opened artifacthost.ArtifactOpenResult
		if err := json.Unmarshal(result, &opened); err != nil || opened.TransferID == "" {
			t.Fatalf("open result=%s err=%v", result, err)
		}
		chunk := artifactWireInvoke(t, conn, 2, "artifact:read", map[string]any{"transferId": opened.TransferID})
		var read artifacthost.ArtifactReadResult
		if err := json.Unmarshal(chunk, &read); err != nil || read.BodyBase64 == "" {
			t.Fatalf("read result=%s err=%v", chunk, err)
		}
		body, err := base64.StdEncoding.DecodeString(read.BodyBase64)
		if err != nil || string(body) != "wire artifact" {
			t.Fatalf("wire body=%q err=%v", body, err)
		}
		closeReply := artifactWireInvoke(t, conn, 3, "artifact:close", map[string]any{"transferId": opened.TransferID})
		var closed map[string]any
		if err := json.Unmarshal(closeReply, &closed); err != nil || closed["closed"] != true {
			t.Fatalf("close result=%s err=%v", closeReply, err)
		}
	}
	deniedLease, err := lease.NewManager(lease.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	deniedHub := wire.NewHub(wire.Options{TrustLocalhost: false, Lease: deniedLease})
	registerArtifactTransferHandlers(deniedHub, reg)
	deniedSrv := httptest.NewServer(http.HandlerFunc(deniedHub.HandleUpgrade))
	defer deniedSrv.Close()
	denied := artifactWireDial(t, deniedSrv.URL)
	defer denied.Close()
	reply := artifactWireInvoke(t, denied, 1, "artifact:open", map[string]any{"path": hosted.URLPath, "method": "GET"})
	var deniedFrame struct {
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(reply, &deniedFrame); err != nil || deniedFrame.Error == nil {
		t.Fatalf("unpaired invoke unexpectedly succeeded: %s", reply)
	}
}

func artifactWireDial(t *testing.T, raw string) net.Conn {
	t.Helper()
	addr := strings.TrimPrefix(raw, "http://")
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: d2lyZS1hcnRpZmFjdA==\r\nSec-WebSocket-Version: 13\r\nX-Workass-Shell: 1\r\n\r\n", addr)
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		c.Close()
		t.Fatalf("websocket handshake: %q %v", line, err)
	}
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return &artifactBufferedConn{Conn: c, r: br}
}

type artifactBufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *artifactBufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func artifactWireInvoke(t *testing.T, c net.Conn, id int, channel string, arg any) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"t": "invoke", "id": id, "channel": channel, "args": []any{arg}})
	if _, err := c.Write(artifactMaskedFrame(body)); err != nil {
		t.Fatal(err)
	}
	for {
		h := make([]byte, 2)
		if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, h); err != nil {
			t.Fatal(err)
		}
		n := int(h[1] & 127)
		if n == 126 {
			b := make([]byte, 2)
			if _, err := io.ReadFull(c, b); err != nil {
				t.Fatal(err)
			}
			n = int(binary.BigEndian.Uint16(b))
		} else if n == 127 {
			b := make([]byte, 8)
			if _, err := io.ReadFull(c, b); err != nil {
				t.Fatal(err)
			}
			v := binary.BigEndian.Uint64(b)
			if v > 16<<20 {
				t.Fatalf("oversize frame %d", v)
			}
			n = int(v)
		}
		if n > 16<<20 {
			t.Fatalf("oversize frame %d", n)
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c, p); err != nil {
			t.Fatal(err)
		}
		var frame struct {
			T      string          `json:"t"`
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *string         `json:"error"`
		}
		json.Unmarshal(p, &frame)
		if frame.T != "reply" || frame.ID != id {
			continue
		}
		if frame.Error != nil {
			return p
		}
		return frame.Result
	}
}
func artifactMaskedFrame(p []byte) []byte {
	out := make([]byte, 0, len(p)+10)
	out = append(out, 0x81)
	if len(p) < 126 {
		out = append(out, byte(0x80|len(p)))
	} else {
		out = append(out, 0x80|126, byte(len(p)>>8), byte(len(p)))
	}
	mask := []byte{1, 2, 3, 4}
	out = append(out, mask...)
	for i, b := range p {
		out = append(out, b^mask[i%4])
	}
	return out
}
