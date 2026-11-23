package notes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/ai"
	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
)

// fakeLLM is an OpenAI-compatible chat completions endpoint.
type fakeLLM struct {
	*httptest.Server
	mu    sync.Mutex
	reqs  []map[string]any
	auth  []string
	title string
	fail  bool
}

func newFakeLLM(t *testing.T, title string) *fakeLLM {
	f := &fakeLLM{title: title}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.reqs = append(f.reqs, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		fail, title := f.fail, f.title
		f.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "nope", "type": "invalid_request_error"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "created": 0, "model": body["model"],
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": title}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

type env struct {
	svc    *Service
	db     *state.DB
	ai     *ai.Settings
	llm    *fakeLLM
	remote string
	cancel context.CancelFunc
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"), AuthorName: "t", AuthorEmail: "t@e", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, _ := secrets.New(make([]byte, 32))
	llm := newFakeLLM(t, `"Pricing follow-up with Acme."`)
	settings := ai.NewSettings(db, box)
	settings.SetBaseURL(llm.URL + "/v1/")
	svc := New(repo, db, settings, quiet)
	svc.SetTiming(50*time.Millisecond, time.Millisecond)
	return &env{svc: svc, db: db, ai: settings, llm: llm, remote: remote}
}

func (e *env) start(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.svc.Start(ctx)
}

// wait polls until the note is no longer pending.
func (e *env) wait(t *testing.T, id string) Note {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, err := e.svc.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !n.Pending {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("note still pending")
	return Note{}
}

func gitShow(t *testing.T, remote, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", remote, "show", rev).CombinedOutput()
	if err != nil {
		t.Fatalf("git show %s: %v %s", rev, err, out)
	}
	return string(out)
}

func TestNoteTitledByModelAndCommitted(t *testing.T) {
	e := setup(t)
	if err := e.ai.Save(context.Background(), "nw-secret-key-1234", "", "antonio"); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.ai.Status(); !st.Configured || st.Model != "nw-flash" || st.KeyHint != "…1234" {
		t.Fatalf("status: %+v", st)
	}

	// Queued before the worker runs (as after a restart): listed as pending.
	id, err := e.svc.Submit("Call with Acme\r\nThey want usage-based pricing.\r\n", "antonio", nil)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.List(); len(list) != 1 || !list[0].Pending || list[0].Title != "" {
		t.Fatalf("pending list: %+v", list)
	}
	if _, err := e.svc.Edit(context.Background(), id, Changes{Title: "x", Body: "y"}, "antonio"); err != ErrPending {
		t.Fatalf("edit while pending: %v", err)
	}

	e.start(t)
	n := e.wait(t, id)
	if n.Title != "Pricing follow-up with Acme" {
		t.Fatalf("title = %q", n.Title)
	}
	if !strings.HasPrefix(n.Path, "notes/") || !strings.HasSuffix(n.Path, "-pricing-follow-up-with-acme-"+id+".md") {
		t.Fatalf("path = %q", n.Path)
	}
	file := gitShow(t, e.remote, "main:"+n.Path)
	if !strings.Contains(file, "title: Pricing follow-up with Acme\n") || !strings.Contains(file, "author: antonio\n") ||
		!strings.HasSuffix(file, "---\nCall with Acme\nThey want usage-based pricing.\n") {
		t.Fatalf("file:\n%s", file)
	}
	if log := gitShow(t, e.remote, "main"); !strings.Contains(log, "Add note: Pricing follow-up with Acme") || !strings.Contains(log, "Note-Id: "+id) {
		t.Fatalf("commit:\n%s", log)
	}

	// What was sent: the configured model, the key, no reasoning, the note.
	e.llm.mu.Lock()
	last := e.llm.reqs[len(e.llm.reqs)-1]
	auth := e.llm.auth[len(e.llm.auth)-1]
	e.llm.mu.Unlock()
	if last["model"] != "nw-flash" || last["reasoning_effort"] != "none" || auth != "Bearer nw-secret-key-1234" {
		t.Fatalf("request: %v %q", last, auth)
	}
	if b, _ := json.Marshal(last["messages"]); !strings.Contains(string(b), "usage-based pricing") {
		t.Fatalf("messages: %s", b)
	}

	// Edit renames the file; delete removes it.
	n, err = e.svc.Edit(context.Background(), id, Changes{Title: "Acme pricing", Body: "Usage-based.\r\n"}, "maxim")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(n.Path, "-acme-pricing-"+id+".md") || n.EditedBy != "maxim" {
		t.Fatalf("edited: %+v", n)
	}
	if got, _ := e.svc.Get(id); got.Body != "Usage-based.\n" || got.Title != "Acme pricing" {
		t.Fatalf("after edit: %+v", got)
	}
	if ls, _ := exec.Command("git", "--git-dir", e.remote, "ls-tree", "-r", "--name-only", "main", "notes").Output(); strings.Count(string(ls), "\n") != 1 {
		t.Fatalf("files after rename: %s", ls)
	}
	if _, err := e.svc.Delete(context.Background(), id, "maxim"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(id); err != ErrNotFound {
		t.Fatalf("after delete: %v", err)
	}
}

func TestNoteFallsBackToFirstLine(t *testing.T) {
	e := setup(t)
	e.start(t)

	// No key: the first line.
	id, _ := e.svc.Submit("\n# Ideas for the demo\n- show calendar linking\n", "maxim", nil)
	if n := e.wait(t, id); n.Title != "Ideas for the demo" {
		t.Fatalf("no key: %q", n.Title)
	}

	// The provider fails: retried, then the first line.
	if err := e.ai.Save(context.Background(), "key-abcdefgh", "nw-flash", "antonio"); err != nil {
		t.Fatal(err)
	}
	e.llm.mu.Lock()
	e.llm.fail = true
	before := len(e.llm.reqs)
	e.llm.mu.Unlock()
	id, _ = e.svc.Submit("Remember to renew the domain", "maxim", nil)
	if n := e.wait(t, id); n.Title != "Remember to renew the domain" {
		t.Fatalf("provider down: %q", n.Title)
	}
	e.llm.mu.Lock()
	tries := len(e.llm.reqs) - before
	e.llm.mu.Unlock()
	if tries < 2 {
		t.Fatalf("tried %d times", tries)
	}

	// A bad key isn't saved.
	if err := e.ai.Save(context.Background(), "another-bad-key", "nw-flash", "antonio"); err == nil {
		t.Fatal("saved a key the provider refused")
	}
	if st, _ := e.ai.Status(); st.KeyHint != "…efgh" {
		t.Fatalf("key replaced: %+v", st)
	}
	if _, err := e.svc.Submit("  \r\n ", "maxim", nil); err == nil {
		t.Fatal("empty note accepted")
	}
}

func TestCleanAndFallbackTitles(t *testing.T) {
	for in, want := range map[string]string{
		`"Q4 hiring plan"`:             "Q4 hiring plan",
		"Title: **Board prep.**\nmore": "Board prep",
		"## Investor update\n":         "Investor update",
	} {
		if got := ai.CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ai.FallbackTitle(strings.Repeat("word ", 30)); len([]rune(got)) > 60 || !strings.HasSuffix(got, "…") {
		t.Errorf("long fallback = %q", got)
	}
	if got := ai.FallbackTitle(" \n "); got != "Untitled note" {
		t.Errorf("empty fallback = %q", got)
	}
}

func TestLabels(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.svc.PutLabel(ctx, "Slop", "Pasted AI output, not reviewed", "antonio"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutLabel(ctx, "competitors", "About other companies in our space", "maxim"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutLabel(ctx, "bad name!", "x", "maxim"); err == nil {
		t.Fatal("invalid name accepted")
	}
	if _, err := e.svc.PutLabel(ctx, "empty", " ", "maxim"); err == nil {
		t.Fatal("label without meaning accepted")
	}
	// Changing a meaning keeps the order.
	if _, err := e.svc.PutLabel(ctx, "slop", "Pasted AI output; don't trust it as fact", "antonio"); err != nil {
		t.Fatal(err)
	}
	file := gitShow(t, e.remote, "main:notes/labels.yaml")
	if !strings.Contains(file, "- name: slop\n  description: Pasted AI output; don't trust it as fact\n- name: competitors\n") {
		t.Fatalf("labels.yaml:\n%s", file)
	}
	if log := gitShow(t, e.remote, "main"); !strings.Contains(log, "Change note label: slop") {
		t.Fatalf("commit:\n%s", log)
	}

	if _, err := e.svc.Submit("text", "maxim", []string{"nope"}); err == nil {
		t.Fatal("undefined label accepted")
	}

	// The model gets the meanings and adds a fitting label; invented ones are dropped.
	if err := e.ai.Save(ctx, "nw-secret-key-1234", "", "antonio"); err != nil {
		t.Fatal(err)
	}
	e.llm.mu.Lock()
	e.llm.title = "Title: Rival pricing pages\nLabels: competitors, invented"
	e.llm.mu.Unlock()
	e.start(t)
	id, err := e.svc.Submit("ChatGPT says Acme charges per seat…", "maxim", []string{"slop", "slop"})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.Get(id); !p.Pending || strings.Join(p.Labels, ",") != "slop" {
		t.Fatalf("pending labels: %+v", p)
	}
	n := e.wait(t, id)
	if n.Title != "Rival pricing pages" || strings.Join(n.Labels, ",") != "slop,competitors" {
		t.Fatalf("note: %q %v", n.Title, n.Labels)
	}
	if file := gitShow(t, e.remote, "main:"+n.Path); !strings.Contains(file, "labels: [slop, competitors]\n") {
		t.Fatalf("file:\n%s", file)
	}
	e.llm.mu.Lock()
	sys, _ := json.Marshal(e.llm.reqs[len(e.llm.reqs)-1]["messages"])
	e.llm.mu.Unlock()
	if !strings.Contains(string(sys), "- slop: Pasted AI output; don't trust it as fact") {
		t.Fatalf("prompt: %s", sys)
	}
	if !n.Matches("competitors") {
		t.Fatal("labels aren't searchable")
	}

	// In use: can't be deleted; counts are right.
	defs, _ := e.svc.LabelsInUse()
	if defs[0].Count != 1 || defs[1].Count != 1 {
		t.Fatalf("counts: %+v", defs)
	}
	if err := e.svc.DeleteLabel(ctx, "slop", "antonio"); !errors.Is(err, ErrLabelInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	// Take it off by editing, then delete.
	if n, err = e.svc.Edit(ctx, id, Changes{Title: n.Title, Body: n.Body, Labels: []string{"competitors"}}, "maxim"); err != nil || strings.Join(n.Labels, ",") != "competitors" {
		t.Fatalf("edit labels: %v %v", n.Labels, err)
	}
	if err := e.svc.DeleteLabel(ctx, "slop", "antonio"); err != nil {
		t.Fatal(err)
	}
	if defs, _ := e.svc.Labels(); len(defs) != 1 || defs[0].Name != "competitors" {
		t.Fatalf("after delete: %+v", defs)
	}
}

func TestSubmitNoteWithTitleAndVia(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// EnsureLabel defines a missing label and leaves an existing one alone.
	if err := e.svc.EnsureLabel(ctx, "slop", "first meaning", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.EnsureLabel(ctx, "slop", "other meaning", "agent"); err != nil {
		t.Fatal(err)
	}
	if defs, _ := e.svc.Labels(); len(defs) != 1 || defs[0].Description != "first meaning" {
		t.Fatalf("labels: %+v", defs)
	}
	if err := e.ai.Save(ctx, "nw-secret-key-1234", "", "antonio"); err != nil {
		t.Fatal(err)
	}
	e.start(t)
	id, err := e.svc.SubmitNote(Submission{Body: "Agent notes.", Author: "antonio", Labels: []string{"slop"}, Title: "  Given\ttitle ", Via: "MCP: Harness"})
	if err != nil {
		t.Fatal(err)
	}
	n := e.wait(t, id)
	if n.Title != "Given title" || n.Via != "MCP: Harness" || strings.Join(n.Labels, ",") != "slop" {
		t.Fatalf("note: %+v", n)
	}
	file := gitShow(t, e.remote, "main:"+n.Path)
	if !strings.Contains(file, "via: 'MCP: Harness'\n") && !strings.Contains(file, "via: \"MCP: Harness\"\n") {
		t.Fatalf("file:\n%s", file)
	}
	if log := gitShow(t, e.remote, "main"); !strings.Contains(log, "Via: MCP: Harness") {
		t.Fatalf("commit:\n%s", log)
	}
	// Editing keeps where it came from.
	if n, err = e.svc.Edit(ctx, id, Changes{Title: "Reviewed", Body: "Agent notes."}, "antonio"); err != nil || n.Via != "MCP: Harness" {
		t.Fatalf("edit: %+v %v", n, err)
	}
}
