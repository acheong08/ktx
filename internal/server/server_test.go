package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/store"
)

const token = "test-token-0123456789abcdef"

const upload = `---
title: Intro call with Acme
date: 2026-11-14T10:00:00-08:00
timezone: America/Los_Angeles
participants:
  - name: Jane Doe
    email: jane@acme.com
source: granola
source_id: not_123
---

**Jane:** Hello.
`

func setup(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{
		RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"),
		AuthorName: "brain-server", AuthorEmail: "brain@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(repo)
	st.SetClock(func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) })
	mux := http.NewServeMux()
	New(st, map[string]string{token: "maxim"}, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, remote, repo.Dir()
}

func post(t *testing.T, ts *httptest.Server, auth, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/transcripts", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/markdown")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestCreateTranscript(t *testing.T) {
	ts, remote, _ := setup(t)

	code, out := post(t, ts, token, upload)
	if code != http.StatusCreated {
		t.Fatalf("status %d: %v", code, out)
	}
	path := out["path"].(string)
	if !strings.HasPrefix(path, "transcripts/unsorted/2026-11-14-1000-intro-call-with-acme-") || out["pushed"] != true || out["result"] != "created" {
		t.Fatalf("response = %v", out)
	}

	file := git(t, "--git-dir", remote, "show", "main:"+path)
	for _, want := range []string{"---\ntitle:", "id: " + out["id"].(string), "ingested_at: \"2026-11-15T12:00:00Z\"", "uploaded_by: maxim", "---\n\n**Jane:** Hello."} {
		if !strings.Contains(file, want) {
			t.Errorf("stored file lacks %q:\n%s", want, file)
		}
	}
	msg := git(t, "--git-dir", remote, "log", "-1", "--format=%B", "main")
	for _, want := range []string{"Add transcript: Intro call with Acme (2026-11-14)", "Uploaded-By: maxim", "Source: granola", "Transcript-Id: "} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message lacks %q:\n%s", want, msg)
		}
	}

	// Identical upload: nothing to commit.
	code, again := post(t, ts, token, upload)
	if code != http.StatusOK || again["result"] != "unchanged" || again["path"] != path {
		t.Fatalf("duplicate upload: %d %v", code, again)
	}
	if n := git(t, "--git-dir", remote, "rev-list", "--count", "main"); n != "1" {
		t.Errorf("want 1 commit, got %s", n)
	}
}

func TestUpdateOverwritesInPlace(t *testing.T) {
	ts, remote, _ := setup(t)
	_, first := post(t, ts, token, upload)

	code, out := post(t, ts, token, strings.Replace(upload, "Hello.", "Hello, corrected.", 1))
	if code != http.StatusOK || out["result"] != "updated" || out["path"] != first["path"] || out["previous_path"] != nil {
		t.Fatalf("update: %d %v", code, out)
	}
	file := git(t, "--git-dir", remote, "show", "main:"+out["path"].(string))
	if !strings.Contains(file, "Hello, corrected.") || !strings.Contains(file, `ingested_at: "2026-11-15T12:00:00Z"`) {
		t.Errorf("stored file:\n%s", file)
	}
	if msg := git(t, "--git-dir", remote, "log", "-1", "--format=%s", "main"); !strings.HasPrefix(msg, "Update transcript:") {
		t.Errorf("subject = %q", msg)
	}
	if n := git(t, "--git-dir", remote, "rev-list", "--count", "main"); n != "2" {
		t.Errorf("want 2 commits, got %s", n)
	}
}

func TestRoutesByType(t *testing.T) {
	ts, remote, _ := setup(t)
	withType := strings.Replace(upload, "source: granola", "type: Customer_Discovery\nsource: granola", 1)

	code, out := post(t, ts, token, withType)
	if code != http.StatusCreated || !strings.HasPrefix(out["path"].(string), "transcripts/customer_discovery/") {
		t.Fatalf("typed upload: %d %v", code, out)
	}

	// Re-upload with another type and title: the file moves and is renamed.
	moved := strings.Replace(strings.Replace(withType, "Customer_Discovery", "advisor", 1), "Intro call with Acme", "Advice from Jane", 1)
	code, out2 := post(t, ts, token, moved)
	if code != http.StatusOK || out2["result"] != "updated" || out2["previous_path"] != out["path"] ||
		!strings.HasPrefix(out2["path"].(string), "transcripts/advisor/2026-11-14-1000-advice-from-jane-") {
		t.Fatalf("retyped upload: %d %v", code, out2)
	}
	tree := git(t, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
	if tree != out2["path"] {
		t.Errorf("remote tree = %q, want only %q", tree, out2["path"])
	}
}

func TestUpdateWithoutTypeKeepsHandSortedFolder(t *testing.T) {
	ts, remote, _ := setup(t)
	_, out := post(t, ts, token, upload)
	name := filepath.Base(out["path"].(string))

	// Antonio sorts it by hand on his laptop and pushes.
	laptop := filepath.Join(t.TempDir(), "laptop")
	git(t, "clone", "--quiet", remote, laptop)
	_ = os.MkdirAll(filepath.Join(laptop, "transcripts", "investor"), 0o755)
	git(t, "-C", laptop, "mv", out["path"].(string), "transcripts/investor/"+name)
	git(t, "-C", laptop, "-c", "user.name=a", "-c", "user.email=a@e", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "sort")
	git(t, "-C", laptop, "push", "--quiet", "origin", "HEAD:main")

	_, again := post(t, ts, token, strings.Replace(upload, "Hello.", "Hi.", 1))
	if again["path"] != "transcripts/investor/"+name || again["result"] != "updated" {
		t.Errorf("response = %v", again)
	}
	if tree := git(t, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); tree != "transcripts/investor/"+name {
		t.Errorf("remote tree = %q", tree)
	}
}

func TestRejects(t *testing.T) {
	ts, _, _ := setup(t)
	if code, _ := post(t, ts, "", upload); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := post(t, ts, "wrong-token-0123456789abcdef", upload); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", code)
	}
	code, out := post(t, ts, token, "+++\ntitle: x\n+++\n")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid: %d", code)
	}
	if details, _ := out["details"].([]any); len(details) != 3 {
		t.Errorf("details = %v", out["details"])
	}
	if code, _ := post(t, ts, token, strings.Repeat("x", 2<<20)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("too big: %d", code)
	}
}

func git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
