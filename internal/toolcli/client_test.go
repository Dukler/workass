package toolcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientRejectsNonlocalAndAmbiguousEndpointsBeforeReadingAuthority(t *testing.T) {
	for _, endpoint := range []string{
		"http://tools.localhost:8788/workass/tools",
		"https://example.com:443/workass/tools",
		"https://tools.localhost/workass/tools",
		"https://owner@tools.localhost:8788/workass/tools",
		"https://tools.localhost:8788/workass/mcp/agent",
		"https://tools.localhost:8788/workass/tools?other=1",
		"https://tools.localhost:8788/workass/tools#fragment",
	} {
		_, err := Do(context.Background(), Config{Endpoint: endpoint, Credential: "test-private-authority"}, "", nil)
		if err == nil || err.Error() != "invalid Workass tool endpoint" {
			t.Fatalf("unsafe endpoint was not rejected before use: %s", endpoint)
		}
		if strings.Contains(err.Error(), "test-private-authority") {
			t.Fatal("error exposed authority")
		}
	}
}

func TestReadConfigRejectsDirectoryOversizedAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadConfig(dir); err == nil {
		t.Fatal("accepted a directory")
	}
	large := filepath.Join(dir, "large.json")
	if err := os.WriteFile(large, make([]byte, 65537), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(large); err == nil {
		t.Fatal("accepted oversized context")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(large, link); err == nil {
		if _, err := ReadConfig(link); err == nil {
			t.Fatal("accepted a symlink")
		}
	}
}
