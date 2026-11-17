package store

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/transcript"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{
		RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"), AuthorName: "t", AuthorEmail: "t@e",
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(repo), remote
}

func put(t *testing.T, s *Store, src string) PutResult {
	t.Helper()
	doc, err := transcript.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Put(context.Background(), doc, "maxim")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

const a = "---\ntitle: Acme intro\ndate: 2026-11-14T10:00:00-08:00\nparticipants: [Jane Doe <jane@acme.com>]\nsource: granola\nsource_id: a\ntags: [pricing]\n---\nWe talked about tenant isolation.\n"
const b = "---\ntitle: Advice\ndate: 2026-11-10T10:00:00Z\ntype: advisor\nparticipants: [Bob]\nsource: granola\nsource_id: b\n---\nHire slowly.\n"

func TestListAndGet(t *testing.T) {
	s, _ := newStore(t)
	ra := put(t, s, a)
	put(t, s, b)

	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Meta.Title != "Acme intro" || entries[1].Folder != "advisor" || entries[0].Folder != "unsorted" {
		t.Fatalf("entries = %+v", entries)
	}
	if !entries[0].Matches("TENANT jane") || !entries[0].Matches("pricing") || entries[0].Matches("tenant bob") {
		t.Error("Matches")
	}
	e, err := s.Get(entries[0].ID)
	if err != nil || e.Path != ra.Path || e.UploadedBy != "maxim" || e.IngestedAt == "" {
		t.Fatalf("Get = %+v, %v", e, err)
	}
	if _, err := s.Get("000000000000"); err != ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestSetType(t *testing.T) {
	s, remote := newStore(t)
	ra := put(t, s, a)
	id := strings.TrimSuffix(ra.Path[strings.LastIndex(ra.Path, "-")+1:], ".md")

	res, err := s.SetType(context.Background(), id, " Customer_Discovery ", "antonio")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != ResultUpdated || res.PreviousPath != ra.Path || !strings.HasPrefix(res.Path, "transcripts/customer_discovery/") || !res.Git.Pushed {
		t.Fatalf("res = %+v", res)
	}
	file := gitOut(t, "--git-dir", remote, "show", "main:"+res.Path)
	if !strings.Contains(file, "type: customer_discovery\n") || !strings.Contains(file, "We talked about tenant isolation.") {
		t.Errorf("file:\n%s", file)
	}
	msg := gitOut(t, "--git-dir", remote, "log", "-1", "--format=%B", "main")
	if !strings.HasPrefix(msg, "Move transcript to customer_discovery: Acme intro (2026-11-14)") || !strings.Contains(msg, "Changed-By: antonio") {
		t.Errorf("message:\n%s", msg)
	}

	if res, _ := s.SetType(context.Background(), id, "customer_discovery", "antonio"); res.Result != ResultUnchanged {
		t.Errorf("same type again: %+v", res)
	}

	res, err = s.SetType(context.Background(), id, "unsorted", "antonio")
	if err != nil || !strings.HasPrefix(res.Path, "transcripts/unsorted/") {
		t.Fatalf("unsort: %+v %v", res, err)
	}
	if file := gitOut(t, "--git-dir", remote, "show", "main:"+res.Path); strings.Contains(file, "type:") {
		t.Errorf("type key should be gone:\n%s", file)
	}

	if _, err := s.SetType(context.Background(), id, "../x", "antonio"); err == nil {
		t.Error("bad type accepted")
	}
	if _, err := s.SetType(context.Background(), "000000000000", "advisor", "antonio"); err != ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestPutWithoutTypeKeepsFolderAndSetsType(t *testing.T) {
	s, remote := newStore(t)
	ra := put(t, s, a)
	id := strings.TrimSuffix(ra.Path[strings.LastIndex(ra.Path, "-")+1:], ".md")
	if _, err := s.SetType(context.Background(), id, "investor", "antonio"); err != nil {
		t.Fatal(err)
	}
	res := put(t, s, strings.Replace(a, "tenant isolation", "billing", 1))
	if res.Result != ResultUpdated || !strings.HasPrefix(res.Path, "transcripts/investor/") {
		t.Fatalf("res = %+v", res)
	}
	if file := gitOut(t, "--git-dir", remote, "show", "main:"+res.Path); !strings.Contains(file, "type: investor") {
		t.Errorf("type not kept:\n%s", file)
	}
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func meeting(t *testing.T, sourceID, title, body string) *transcript.Doc {
	t.Helper()
	doc, err := transcript.Parse([]byte("---\ntitle: " + title + "\ndate: 2026-11-15T17:00:00Z\nparticipants: [Antonio <ac@melzi.dev>]\nsource: granola\nsource_id: " + sourceID + "\n---\n" + body + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestPutBatchDefaultType(t *testing.T) {
	s, remote := newStore(t)
	ctx := context.Background()
	items := []Item{
		{Doc: meeting(t, "not_1", "Acme intro", "hello"), DefaultType: "meetings"},
		{Doc: meeting(t, "not_2", "Board prep", "numbers"), DefaultType: "meetings"},
	}
	res, git, err := s.PutBatch(ctx, items, "antonio", "Sync granola", "Connection: granola/Antonio")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Result != ResultCreated || !strings.HasPrefix(res[0].Path, "transcripts/meetings/2026-11-15-1700-acme-intro-") || !git.Pushed {
		t.Fatalf("results: %+v %+v", res, git)
	}
	if n := gitOut(t, "--git-dir", remote, "rev-list", "--count", "main"); n != "1" {
		t.Errorf("want one commit for the batch, got %s", n)
	}
	msg := gitOut(t, "--git-dir", remote, "log", "-1", "--format=%B", "main")
	for _, want := range []string{"Sync granola: 2 added, 0 updated", "- created transcripts/meetings/", "Uploaded-By: antonio", "Connection: granola/Antonio"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if file := gitOut(t, "--git-dir", remote, "show", "main:"+res[0].Path); !strings.Contains(file, "type: meetings\n") {
		t.Errorf("type not written:\n%s", file)
	}

	// Sorted by hand, then updated by the connector: it stays where it was put.
	entries, _ := s.List()
	var acme Entry
	for _, e := range entries {
		if e.Meta.Title == "Acme intro" {
			acme = e
		}
	}
	if _, err := s.SetType(ctx, acme.ID, "customer_discovery", "maxim"); err != nil {
		t.Fatal(err)
	}
	items[0].Doc = meeting(t, "not_1", "Acme intro", "hello, edited")
	items[1].Doc = meeting(t, "not_2", "Board prep", "numbers")
	res, _, err = s.PutBatch(ctx, items, "antonio", "Sync granola")
	if err != nil || res[0].Result != ResultUpdated || !strings.HasPrefix(res[0].Path, "transcripts/customer_discovery/") || res[1].Result != ResultUnchanged {
		t.Fatalf("re-sync: %+v %v", res, err)
	}
	if file := gitOut(t, "--git-dir", remote, "show", "main:"+res[0].Path); !strings.Contains(file, "type: customer_discovery") {
		t.Errorf("hand-sorted type lost:\n%s", file)
	}
	if _, _, err := s.PutBatch(ctx, []Item{{Doc: items[0].Doc, DefaultType: "../x"}}, "a", ""); err == nil {
		t.Error("bad default type accepted")
	}
}

func TestEdit(t *testing.T) {
	s, remote := newStore(t)
	ctx := context.Background()
	ra := put(t, s, a) // Acme intro, unsorted, granola source
	id := strings.TrimSuffix(ra.Path[strings.LastIndex(ra.Path, "-")+1:], ".md")

	res, err := s.Edit(ctx, id, "maxim", func(d *transcript.Doc) error {
		d.Set("title", "Acme discovery call")
		d.Set("type", "Customer_Discovery")
		d.Body = "Corrected notes.\n"
		return d.SetValue("participants", []transcript.Participant{{Name: "Jane Doe", Email: "jane@acme.com", Role: "CTO"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != ResultUpdated || res.PreviousPath != ra.Path || !strings.HasPrefix(res.Path, "transcripts/customer_discovery/2026-11-14-1000-acme-discovery-call-"+id) {
		t.Fatalf("res = %+v", res)
	}
	file := gitOut(t, "--git-dir", remote, "show", "main:"+res.Path)
	for _, want := range []string{"title: Acme discovery call", "type: customer_discovery", "role: CTO", "source_id: a", "id: " + id, "ingested_at:", "Corrected notes."} {
		if !strings.Contains(file, want) {
			t.Errorf("file lacks %q:\n%s", want, file)
		}
	}
	if strings.Contains(file, "tenant isolation") {
		t.Error("old body kept")
	}
	msg := gitOut(t, "--git-dir", remote, "log", "-1", "--format=%B", "main")
	if !strings.HasPrefix(msg, "Edit transcript: Acme discovery call (2026-11-14)") || !strings.Contains(msg, "Changed-By: maxim") {
		t.Errorf("message:\n%s", msg)
	}
	if n := gitOut(t, "--git-dir", remote, "rev-list", "--count", "main"); n != "2" {
		t.Errorf("commits = %s", n)
	}

	// Nothing changed: no commit. Breaking the rules: refused, nothing written.
	if res, err := s.Edit(ctx, id, "maxim", func(d *transcript.Doc) error { return nil }); err != nil || res.Result != ResultUnchanged {
		t.Errorf("no-op edit: %+v %v", res, err)
	}
	_, err = s.Edit(ctx, id, "maxim", func(d *transcript.Doc) error { d.Body = " \n"; d.Unset("participants"); return nil })
	var verr *transcript.ValidationError
	if !errors.As(err, &verr) || len(verr.Problems) != 2 {
		t.Errorf("invalid edit: %v", err)
	}
	if n := gitOut(t, "--git-dir", remote, "rev-list", "--count", "main"); n != "2" {
		t.Errorf("invalid edit committed something: %s commits", n)
	}
	if _, err := s.Edit(ctx, "000000000000", "maxim", func(*transcript.Doc) error { return nil }); err != ErrNotFound {
		t.Errorf("missing: %v", err)
	}
}

func TestDelete(t *testing.T) {
	s, remote := newStore(t)
	ctx := context.Background()
	ra := put(t, s, a)
	id := strings.TrimSuffix(ra.Path[strings.LastIndex(ra.Path, "-")+1:], ".md")

	if _, err := s.Delete(ctx, id, "maxim"); err != nil {
		t.Fatal(err)
	}
	tree := gitOut(t, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
	if tree != DeletedFile {
		t.Errorf("tree after delete = %q", tree)
	}
	msg := gitOut(t, "--git-dir", remote, "log", "-1", "--format=%B", "main")
	if !strings.HasPrefix(msg, "Delete transcript: Acme intro (2026-11-14)") || !strings.Contains(msg, "Changed-By: maxim") {
		t.Errorf("message:\n%s", msg)
	}
	if !strings.Contains(gitOut(t, "--git-dir", remote, "show", "main:"+DeletedFile), id+" ") {
		t.Error("id not recorded")
	}
	if _, err := s.Get(id); err != ErrNotFound {
		t.Errorf("still readable: %v", err)
	}
	if _, err := s.Delete(ctx, id, "maxim"); err != ErrNotFound {
		t.Errorf("second delete: %v", err)
	}

	// A connector sync of it is skipped; nothing is committed.
	doc, _ := transcript.Parse([]byte(a))
	res, _, err := s.PutBatch(ctx, []Item{{Doc: doc, Sync: true}}, "antonio", "Sync granola")
	if err != nil || res[0].Result != ResultSkipped {
		t.Fatalf("sync after delete: %+v %v", res, err)
	}
	if n := gitOut(t, "--git-dir", remote, "rev-list", "--count", "main"); n != "2" {
		t.Errorf("commits = %s", n)
	}

	// A person re-uploading brings it back and clears the record.
	again := put(t, s, a)
	if again.Result != ResultCreated {
		t.Fatalf("re-upload: %+v", again)
	}
	if strings.Contains(gitOut(t, "--git-dir", remote, "show", "main:"+DeletedFile), id) {
		t.Error("record not cleared")
	}
	doc, _ = transcript.Parse([]byte(a))
	if res, _, _ := s.PutBatch(ctx, []Item{{Doc: doc, Sync: true}}, "maxim", ""); res[0].Result != ResultUnchanged {
		t.Errorf("sync after re-upload: %+v", res)
	}
}

func TestEditedTranscriptSurvivesSync(t *testing.T) {
	s, remote := newStore(t)
	ctx := context.Background()
	doc, _ := transcript.Parse([]byte(a))
	res, _, err := s.PutBatch(ctx, []Item{{Doc: doc, Sync: true}}, "antonio", "Sync pocket")
	if err != nil {
		t.Fatal(err)
	}
	id := doc.ID

	// A no-op edit doesn't mark it.
	if r, _ := s.Edit(ctx, id, "maxim", func(*transcript.Doc) error { return nil }); r.Result != ResultUnchanged {
		t.Fatalf("no-op: %+v", r)
	}
	if _, err := s.Edit(ctx, id, "maxim", func(d *transcript.Doc) error { d.Body = "Fixed by hand.\n"; return nil }); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(id)
	if e.EditedBy != "maxim" || e.EditedAt == "" {
		t.Fatalf("not marked: %+v", e)
	}

	// The source changes: the sync leaves the edited file alone.
	changed, _ := transcript.Parse([]byte(strings.Replace(a, "tenant isolation", "something new", 1)))
	got, _, err := s.PutBatch(ctx, []Item{{Doc: changed, Sync: true}}, "antonio", "Sync pocket")
	if err != nil || got[0].Result != ResultSkipped || got[0].Path != res[0].Path {
		t.Fatalf("sync after edit: %+v %v", got, err)
	}
	if file := gitOut(t, "--git-dir", remote, "show", "main:"+res[0].Path); !strings.Contains(file, "Fixed by hand.") {
		t.Errorf("edit lost:\n%s", file)
	}

	// An upload by a person still replaces it.
	changed, _ = transcript.Parse([]byte(strings.Replace(a, "tenant isolation", "something new", 1)))
	if r, err := s.Put(ctx, changed, "antonio"); err != nil || r.Result != ResultUpdated {
		t.Errorf("manual upload: %+v %v", r, err)
	}
}
