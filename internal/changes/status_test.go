package changes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseStatusV2BranchAheadBehind(t *testing.T) {
	cases := []struct {
		name              string
		headers           string
		branch, upstream  string
		detached, abKnown bool
		ahead, behind     int
	}{
		{"ahead", "# branch.oid abc\x00# branch.head main\x00# branch.upstream origin/main\x00# branch.ab +2 -0\x00", "main", "origin/main", false, true, 2, 0},
		{"behind", "# branch.oid abc\x00# branch.head feature/steer\x00# branch.upstream origin/feature/steer\x00# branch.ab +0 -1\x00", "feature/steer", "origin/feature/steer", false, true, 0, 1},
		{"diverged", "# branch.oid abc\x00# branch.head main\x00# branch.upstream origin/main\x00# branch.ab +12 -34\x00", "main", "origin/main", false, true, 12, 34},
		{"no upstream", "# branch.oid abc\x00# branch.head scratch\x00", "scratch", "", false, false, 0, 0},
		// The upstream branch was deleted: git prints no ab line, so the
		// counts are unknown rather than zero.
		{"upstream gone", "# branch.oid abc\x00# branch.head old\x00# branch.upstream origin/old\x00", "old", "origin/old", false, false, 0, 0},
		{"detached", "# branch.oid abc\x00# branch.head (detached)\x00", "", "", true, false, 0, 0},
		{"garbled ab", "# branch.oid abc\x00# branch.head main\x00# branch.ab 2 1\x00", "main", "", false, false, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStatusV2([]byte(tc.headers))
			if got.branch != tc.branch || got.upstream != tc.upstream || got.detached != tc.detached ||
				got.abKnown != tc.abKnown || got.ahead != tc.ahead || got.behind != tc.behind {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestParseStatusV2Entries(t *testing.T) {
	out := "# branch.oid (initial)\x00# branch.head main\x00" +
		"1 .M N... 100644 100644 100644 aaa bbb dir with space/file name.go\x00" +
		"1 A. N... 000000 100644 100644 000 bbb added.go\x00" +
		"2 R. N... 100644 100644 100644 aaa bbb R100 new/name.go\x00old/name.go\x00" +
		"u UU N... 100644 100644 100644 100644 a b c conflict.go\x00" +
		"? untracked dir/new.txt\x00" +
		"! ignored.log\x00"
	got := parseStatusV2([]byte(out))
	if !got.initial {
		t.Fatal("initial commit not seen")
	}
	want := []fileStatus{
		{code: ".M", path: "dir with space/file name.go"},
		{code: "A.", path: "added.go"},
		{code: "R.", path: "new/name.go", orig: "old/name.go"},
		{code: "UU", path: "conflict.go"},
		{code: "??", path: "untracked dir/new.txt", untracked: true},
	}
	if len(got.files) != len(want) {
		t.Fatalf("files %+v", got.files)
	}
	for index := range want {
		if got.files[index] != want[index] {
			t.Fatalf("file %d = %+v want %+v", index, got.files[index], want[index])
		}
	}
}

func TestParseNumstatZ(t *testing.T) {
	stats := parseNumstatZ([]byte("18\t3\tui/Queue.tsx\x00-\t-\tlogo.png\x000\t7\tgone.go\x005\t1\t\x00old.go\x00new.go\x00"))
	if stats["ui/Queue.tsx"] != (numstat{adds: 18, dels: 3}) || !stats["logo.png"].binary || stats["gone.go"].dels != 7 || stats["new.go"].adds != 5 {
		t.Fatalf("numstat %+v", stats)
	}
}

func TestUntrackedLinesAreCountedWithoutGit(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"three.txt": "a\nb\nc\n", "no-newline.txt": "a\nb", "empty.txt": "", "bin.dat": "ab\x00cd"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	status := repoStatus{files: []fileStatus{
		{path: "three.txt", untracked: true}, {path: "no-newline.txt", untracked: true},
		{path: "empty.txt", untracked: true}, {path: "bin.dat", untracked: true}, {path: "missing.txt", untracked: true},
	}}
	status.countUntracked(root)
	got := map[string]int{}
	for _, file := range status.files {
		got[file.path] = file.adds
	}
	if got["three.txt"] != 3 || got["no-newline.txt"] != 2 || got["empty.txt"] != 0 || got["bin.dat"] != 0 || got["missing.txt"] != 0 || !status.files[3].binary {
		t.Fatalf("counts %v %+v", got, status.files)
	}
}

func TestEditPaths(t *testing.T) {
	cwd := filepath.FromSlash("/work/proj")
	cases := []struct {
		location, input string
		want            []string
	}{
		{"/work/proj/a.go:42", "", []string{"/work/proj/a.go"}},
		{"src/b.ts", "", []string{"/work/proj/src/b.ts"}},
		{"", `{"file_path":"/work/proj/c.go","old_string":"x"}`, []string{"/work/proj/c.go"}},
		{"/work/proj/d.go", `{"changes":[{"path":"/work/proj/d.go"},{"path":"/work/proj/e.go"}]}`, []string{"/work/proj/d.go", "/work/proj/e.go"}},
		// Cut at 400 characters: no longer JSON, the path is still found.
		{"", `{"file_path":"/work/proj/f.go","new_string":"func f() {`, []string{"/work/proj/f.go"}},
		{"mock://workspace", `{"path":"mock://workspace"}`, nil},
	}
	for _, tc := range cases {
		got := EditPaths(tc.location, tc.input, cwd)
		if len(got) != len(tc.want) {
			t.Fatalf("EditPaths(%q, %q) = %v want %v", tc.location, tc.input, got, tc.want)
		}
		for index := range got {
			if got[index] != filepath.FromSlash(tc.want[index]) {
				t.Fatalf("EditPaths(%q, %q) = %v want %v", tc.location, tc.input, got, tc.want)
			}
		}
	}
	if !IsEditTool("edit", "completed") || !IsEditTool("delete", "") || IsEditTool("edit", "failed") || IsEditTool("read", "completed") {
		t.Fatal("IsEditTool")
	}
}
