package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSessionImageNameMemoIsByteBoundedAndNotPayloadKeyed(t *testing.T) {
	const payloadSize = 9 << 20
	for index := 0; index < 8; index++ {
		sessionImageName(strings.Repeat(string(rune('a'+index)), payloadSize))
	}

	sessionImageNameMu.Lock()
	defer sessionImageNameMu.Unlock()
	memoType := reflect.TypeOf(sessionImageNameMemo)
	if memoType.Kind() == reflect.Map && memoType.Key().Kind() == reflect.String {
		t.Fatal("session image memo still keys its long-lived map on complete payload strings")
	}
	retained := reflectedStringBytes(reflect.ValueOf(sessionImageNameMemo), map[uintptr]struct{}{})
	if retained > 64<<20 {
		t.Fatalf("session image memo retains %d string bytes, want at most 64 MiB", retained)
	}
}

func reflectedStringBytes(value reflect.Value, seen map[uintptr]struct{}) int {
	if !value.IsValid() {
		return 0
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return 0
		}
		return reflectedStringBytes(value.Elem(), seen)
	case reflect.Pointer:
		if value.IsNil() {
			return 0
		}
		pointer := value.Pointer()
		if _, ok := seen[pointer]; ok {
			return 0
		}
		seen[pointer] = struct{}{}
		return reflectedStringBytes(value.Elem(), seen)
	case reflect.String:
		return value.Len()
	case reflect.Map:
		total := 0
		iter := value.MapRange()
		for iter.Next() {
			total += reflectedStringBytes(iter.Key(), seen)
			total += reflectedStringBytes(iter.Value(), seen)
		}
		return total
	case reflect.Slice, reflect.Array:
		total := 0
		for index := 0; index < value.Len(); index++ {
			total += reflectedStringBytes(value.Index(index), seen)
		}
		return total
	case reflect.Struct:
		total := 0
		for index := 0; index < value.NumField(); index++ {
			total += reflectedStringBytes(value.Field(index), seen)
		}
		return total
	default:
		return 0
	}
}

func imageReadFixture(tb testing.TB) (string, string, string) {
	tb.Helper()
	root := tb.TempDir()
	data := "data:image/png;base64," + strings.Repeat("A", 1<<20)
	name := sessionImageName(data)
	if err := os.MkdirAll(filepath.Join(root, sessionImageDirname), 0700); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sessionImageDirname, name), []byte(data), 0600); err != nil {
		tb.Fatal(err)
	}
	return root, sessionImageDirname + "/" + name, data
}

func repeatedImageProjection(ref string) []any {
	rows := make([]any, 8)
	for i := range rows {
		rows[i] = map[string]any{sessionImageDataRefField: ref, "mimeType": "image/png"}
	}
	return rows
}

func BenchmarkRehydrateRepeatedSessionImage(b *testing.B) {
	root, ref, _ := imageReadFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := rehydrateExternalSessionImages(repeatedImageProjection(ref), root); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRepeatedSessionImagesPreservePayloadAndVerifyEachNewRead(t *testing.T) {
	root, ref, data := imageReadFixture(t)
	rows := repeatedImageProjection(ref)
	if err := rehydrateExternalSessionImages(rows, root); err != nil {
		t.Fatal(err)
	}
	for _, raw := range rows {
		image := raw.(map[string]any)
		if image["data"] != data {
			t.Fatal("image payload changed")
		}
		if _, ok := image[sessionImageDataRefField]; ok {
			t.Fatal("internal ref leaked")
		}
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(ref)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	// Deduplication must be scoped to this projection, never an unchecked
	// process-lifetime cache that conceals a damaged or replaced sidecar.
	rows = repeatedImageProjection(ref)
	if err := rehydrateExternalSessionImages(rows, root); err == nil {
		t.Fatal("new read reused an unverified old payload")
	}
	for _, raw := range rows {
		image := raw.(map[string]any)
		if _, ok := image["data"]; ok {
			t.Fatal("broken sidecar was rendered")
		}
	}
}
