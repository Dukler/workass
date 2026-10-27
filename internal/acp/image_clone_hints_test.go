package acp

import (
	"reflect"
	"testing"
)

func TestToolImageCloneSourcesAreHintsOnCopies(t *testing.T) {
	original := map[string]any{"mimeType": "image/png", "data": "AAAA"}
	images := []any{original}
	hinted := withToolImageCloneSources(images, `{"path":"/Users/me/Workspace/a.png","note":"relative/b.png"}`)
	if _, leaked := original[ImageCloneSourceField]; leaked {
		t.Fatal("the wire's image map must not change")
	}
	copied := hinted[0].(map[string]any)
	if got := copied[ImageCloneSourceField]; !reflect.DeepEqual(got, []any{"/Users/me/Workspace/a.png"}) {
		t.Fatalf("hints = %v", got)
	}
	if copied["data"] != "AAAA" {
		t.Fatal("copy lost the image")
	}
	if same := withToolImageCloneSources(images, `{"command":"ls"}`); &same[0] != &images[0] {
		t.Fatal("no path means the images pass through untouched")
	}
}

func TestToolInputImagePathsSurviveTruncatedInput(t *testing.T) {
	truncated := `{"file_path":"/tmp/shot.png","content":"` + string(make([]byte, 10)) // compact input cut mid-value
	if got := ToolInputImagePaths(truncated); !reflect.DeepEqual(got, []string{"/tmp/shot.png"}) {
		t.Fatalf("got %v", got)
	}
	nested := `{"arguments":{"paths":["/a/1.png","/a/2.png"],"path":"/a/1.png"},"server":"x"}`
	if got := ToolInputImagePaths(nested); !reflect.DeepEqual(got, []string{"/a/1.png", "/a/2.png"}) {
		t.Fatalf("got %v", got)
	}
}
