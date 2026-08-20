package acp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestChatCheckpointsDiffRewindAndOutsideGuard(t *testing.T) {
	requireGit(t)
	workspace := t.TempDir()
	repoDir := filepath.Join(workspace, "alpha")
	initTinyGitRepo(t, repoDir, map[string]string{"work.txt": "one\n"})
	stateDir := filepath.Join(t.TempDir(), "state")
	manager, events := newFakeManager(t, "slow-prompt", Options{StateDir: stateDir, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{CWD: workspace, TabID: "cp-tab", ChatID: "chat-cp"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	_ = waitChatEnv(t, events, func(env ChatEnvPayload) bool {
		return env.ChatID == "chat-cp" && strings.Join(env.Unchanged, ",") == "alpha"
	}, 2*time.Second)

	workPath := filepath.Join(repoDir, "work.txt")
	hashA := fileSHA256(t, workPath)
	job1, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind:      "app-chat",
		SessionID: session.SessionID,
		ChatID:    "chat-cp",
		TabID:     "cp-tab",
		CWD:       workspace,
		Prompt:    "turn one",
	})
	if err != nil {
		t.Fatalf("start job1: %v", err)
	}
	writeFile(t, workPath, "one\ntwo\nthree\n")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job1), 2*time.Second), "done", 0, "end_turn")
	hashB := fileSHA256(t, workPath)

	job2, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind:      "app-chat",
		SessionID: session.SessionID,
		ChatID:    "chat-cp",
		TabID:     "cp-tab",
		CWD:       workspace,
		Prompt:    "turn two",
	})
	if err != nil {
		t.Fatalf("start job2: %v", err)
	}
	writeFile(t, workPath, "one\ntwo\nthree\nfour\n")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job2), 2*time.Second), "done", 0, "end_turn")
	hashC := fileSHA256(t, workPath)

	checkpoints := manager.ChatCheckpoints("chat-cp", "")
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoints = %#v", checkpoints)
	}
	if checkpoints[0].TurnSeq != 1 || checkpoints[1].TurnSeq != 2 || checkpoints[0].Repos[0].Skipped || checkpoints[1].Repos[0].Skipped {
		t.Fatalf("checkpoint metadata = %#v", checkpoints)
	}
	if checkpoints[0].Repos[0].ChangedFiles != 1 || checkpoints[1].Repos[0].ChangedFiles != 1 {
		t.Fatalf("changed file counts = %#v", checkpoints)
	}
	verifyGitRef(t, repoDir, checkpoints[0].Repos[0].Ref, checkpoints[0].Repos[0].Commit)
	verifyGitRef(t, repoDir, checkpoints[1].Repos[0].Ref, checkpoints[1].Repos[0].Commit)
	t.Logf("trace checkpoint recorded chat=chat-cp turnSeqs=%d,%d refs=%s,%s hashes=%s,%s,%s", checkpoints[0].TurnSeq, checkpoints[1].TurnSeq, checkpoints[0].Repos[0].Ref, checkpoints[1].Repos[0].Ref, hashA, hashB, hashC)

	diff, err := manager.ChatDiff(context.Background(), "chat-cp", "alpha", "work.txt")
	if err != nil {
		t.Fatalf("chat diff: %v", err)
	}
	diffText := diff["text"].(string)
	if diff["truncated"] != false || !strings.Contains(diffText, "@@") || !strings.Contains(diffText, "+four") {
		t.Fatalf("diff result = %#v", diff)
	}
	t.Logf("trace diff chat=chat-cp repo=alpha path=work.txt turnSeq=%v contains=%q", diff["turnSeq"], "+four")

	restore := func(turnSeq int, operationID providercontract.OperationID) (map[string]any, error) {
		checkpoint, ok := checkpointByTurn(checkpoints, turnSeq)
		if !ok {
			t.Fatalf("checkpoint %d disappeared", turnSeq)
		}
		payload, err := json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		return manager.RestoreChatCheckpoint(context.Background(), "chat-cp", turnSeq, payload, fmt.Sprintf("%x", digest), operationID)
	}
	if _, err := restore(1, "rewind-turn-1"); err != nil {
		t.Fatalf("rewind turn 1: %v", err)
	}
	if got := fileSHA256(t, workPath); got != hashA {
		t.Fatalf("rewind turn1 hash=%s want=%s", got, hashA)
	}
	if _, err := restore(2, "rewind-turn-2"); err != nil {
		t.Fatalf("rewind turn 2: %v", err)
	}
	if got := fileSHA256(t, workPath); got != hashB {
		t.Fatalf("rewind turn2 hash=%s want=%s", got, hashB)
	}
	t.Logf("trace rewind hashes turn1=%s turn2=%s", hashA, hashB)

	writeFile(t, workPath, "outside\n")
	_, err = restore(1, "rewind-outside-modification")
	if err == nil || structuredErrorCode(err) != "chat:rewind-outside-modification" {
		t.Fatalf("outside modification error = %v code=%s", err, structuredErrorCode(err))
	}
	t.Logf("trace rewind refused code=%s", structuredErrorCode(err))
}

func TestChatCheckpointRotationAndLargeRepoSkip(t *testing.T) {
	requireGit(t)
	t.Run("rotation", func(t *testing.T) {
		workspace := t.TempDir()
		repoDir := filepath.Join(workspace, "alpha")
		initTinyGitRepo(t, repoDir, map[string]string{"work.txt": "base\n"})
		manager, events := newFakeManager(t, "slow-prompt", Options{StateDir: filepath.Join(t.TempDir(), "state"), RSSSampleInterval: time.Hour})
		t.Cleanup(func() { manager.Reset() })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		session, err := manager.NewSession(ctx, SessionOptions{CWD: workspace, TabID: "rotate-tab", ChatID: "chat-rotate"})
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		_ = waitChatEnv(t, events, func(env ChatEnvPayload) bool { return env.ChatID == "chat-rotate" }, 2*time.Second)
		for i := 1; i <= checkpointLimit+1; i++ {
			job, err := manager.StartJob(context.Background(), JobStartOptions{
				Kind:      "app-chat",
				SessionID: session.SessionID,
				ChatID:    "chat-rotate",
				TabID:     "rotate-tab",
				CWD:       workspace,
				Prompt:    fmt.Sprintf("turn %d", i),
			})
			if err != nil {
				t.Fatalf("start rotation job %d: %v", i, err)
			}
			writeFile(t, filepath.Join(repoDir, "work.txt"), "base\n"+strings.Repeat("turn\n", i))
			assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
		}
		checkpoints := manager.ChatCheckpoints("chat-rotate", "")
		if len(checkpoints) != checkpointLimit || checkpoints[0].TurnSeq != 2 || checkpoints[len(checkpoints)-1].TurnSeq != checkpointLimit+1 {
			t.Fatalf("rotated checkpoints = %#v", checkpoints)
		}
		if gitRefExists(t, repoDir, checkpointRef("chat-rotate", 1)) {
			t.Fatalf("rotated ref still exists")
		}
		if !gitRefExists(t, repoDir, checkpointRef("chat-rotate", checkpointLimit+1)) {
			t.Fatalf("latest ref missing")
		}
		t.Logf("trace checkpoint rotation count=%d first=%d last=%d", len(checkpoints), checkpoints[0].TurnSeq, checkpoints[len(checkpoints)-1].TurnSeq)
	})

	t.Run("large repo skip", func(t *testing.T) {
		workspace := t.TempDir()
		repoDir := filepath.Join(workspace, "large")
		files := map[string]string{}
		for i := 0; i < checkpointFileLimit+1; i++ {
			files[fmt.Sprintf("file%03d.txt", i)] = "base\n"
		}
		initTinyGitRepo(t, repoDir, files)
		manager, events := newFakeManager(t, "slow-prompt", Options{StateDir: filepath.Join(t.TempDir(), "state"), RSSSampleInterval: time.Hour})
		t.Cleanup(func() { manager.Reset() })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		session, err := manager.NewSession(ctx, SessionOptions{CWD: repoDir, TabID: "large-tab", ChatID: "chat-large"})
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		_ = waitChatEnv(t, events, func(env ChatEnvPayload) bool { return env.ChatID == "chat-large" }, 2*time.Second)
		job, err := manager.StartJob(context.Background(), JobStartOptions{
			Kind:      "app-chat",
			SessionID: session.SessionID,
			ChatID:    "chat-large",
			TabID:     "large-tab",
			CWD:       repoDir,
			Prompt:    "large change",
		})
		if err != nil {
			t.Fatalf("start large job: %v", err)
		}
		for i := 0; i < checkpointFileLimit+1; i++ {
			writeFile(t, filepath.Join(repoDir, fmt.Sprintf("file%03d.txt", i)), "base\nchanged\n")
		}
		assertJobStatus(t, events.waitJobEnd(t, jobID(job), 3*time.Second), "done", 0, "end_turn")
		checkpoints := manager.ChatCheckpoints("chat-large", "")
		if len(checkpoints) != 1 || len(checkpoints[0].Repos) != 1 {
			t.Fatalf("large checkpoints = %#v", checkpoints)
		}
		repo := checkpoints[0].Repos[0]
		if !repo.Skipped || repo.SkipReason != "changed-file-limit" || repo.ChangedFiles != checkpointFileLimit+1 {
			t.Fatalf("large checkpoint repo = %#v", repo)
		}
		if gitRefExists(t, repoDir, repo.Ref) {
			t.Fatalf("skipped large ref exists: %s", repo.Ref)
		}
		t.Logf("trace checkpoint skipped repo=%s changedFiles=%d reason=%s", repo.Name, repo.ChangedFiles, repo.SkipReason)
	})
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hash file: %v", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func verifyGitRef(t *testing.T, repo, ref, wantCommit string) {
	t.Helper()
	out, err := gitOutput(context.Background(), repo, "rev-parse", "--verify", ref)
	if err != nil {
		t.Fatalf("verify ref %s: %v", ref, err)
	}
	got := strings.TrimSpace(string(out))
	if got != wantCommit {
		t.Fatalf("ref %s = %s want %s", ref, got, wantCommit)
	}
}

func gitRefExists(t *testing.T, repo, ref string) bool {
	t.Helper()
	_, err := gitOutput(context.Background(), repo, "rev-parse", "--verify", ref)
	return err == nil
}

func structuredErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(err.Error()), &payload) != nil {
		return ""
	}
	code, _ := payload["code"].(string)
	return code
}
