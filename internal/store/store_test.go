package store

import (
	"context"
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

func TestPutBatchMeetings(t *testing.T) {
	s, remote := newStore(t)
	ctx := context.Background()
	items := []Item{
		{Doc: meeting(t, "not_1", "Acme intro", "hello"), Collection: Meetings},
		{Doc: meeting(t, "not_2", "Board prep", "numbers"), Collection: Meetings},
	}
	res, git, err := s.PutBatch(ctx, items, "antonio", "Sync granola", "Connection: granola/Antonio")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Result != ResultCreated || !strings.HasPrefix(res[0].Path, "meetings/2026-11-15-1700-acme-intro-") || !git.Pushed {
		t.Fatalf("results: %+v %+v", res, git)
	}
	if n := gitOut(t, "--git-dir", remote, "rev-list", "--count", "main"); n != "1" {
		t.Errorf("want one commit for the batch, got %s", n)
	}
	msg := gitOut(t, "--git-dir", remote, "log", "-1", "--format=%B", "main")
	for _, want := range []string{"Sync granola: 2 added, 0 updated", "- created meetings/", "Uploaded-By: antonio", "Connection: granola/Antonio"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}

	// Same batch again: nothing to commit. One changed: a single-item message.
	items[0].Doc = meeting(t, "not_1", "Acme intro", "hello")
	items[1].Doc = meeting(t, "not_2", "Board prep", "numbers, revised")
	res, _, err = s.PutBatch(ctx, items, "antonio", "Sync granola")
	if err != nil || res[0].Result != ResultUnchanged || res[1].Result != ResultUpdated {
		t.Fatalf("second batch: %+v %v", res, err)
	}
	if msg := gitOut(t, "--git-dir", remote, "log", "-1", "--format=%s", "main"); !strings.HasPrefix(msg, "Update meeting: Board prep") {
		t.Errorf("subject = %q", msg)
	}

	entries, err := s.List()
	if err != nil || len(entries) != 2 || entries[0].Collection != Meetings || entries[0].Folder != "meetings" {
		t.Fatalf("list: %+v %v", entries, err)
	}
	if _, err := s.SetType(ctx, entries[0].ID, "advisor", "antonio"); err != ErrNotSortable {
		t.Errorf("SetType on a meeting: %v", err)
	}
	if _, _, err := s.PutBatch(ctx, []Item{{Doc: items[0].Doc, Collection: "elsewhere"}}, "a", ""); err == nil {
		t.Error("unknown collection accepted")
	}
}
