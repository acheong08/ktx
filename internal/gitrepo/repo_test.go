package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRemote creates a bare repository, optionally with one commit on main.
func newRemote(t *testing.T, seed bool) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	run(t, dir, "git", "init", "--quiet", "--bare", "-b", "main", bare)
	if seed {
		seedDir := filepath.Join(dir, "seed")
		run(t, dir, "git", "clone", "--quiet", bare, seedDir)
		writeFile(t, seedDir, "README.md", "seed\n")
		commitAll(t, seedDir, "seed")
		run(t, seedDir, "git", "push", "--quiet", "origin", "HEAD:main")
	}
	return bare
}

func open(t *testing.T, remote string) *Repo {
	t.Helper()
	r, err := Open(context.Background(), Options{
		RemoteURL: remote, Branch: "main", Dir: filepath.Join(t.TempDir(), "work"),
		AuthorName: "test", AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func addFile(name, content string) func(string) (Change, error) {
	return func(root string) (Change, error) {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return Change{}, err
		}
		return Change{Paths: []string{name}, Message: "add " + name}, os.WriteFile(p, []byte(content), 0o644)
	}
}

func TestUpdatePushes(t *testing.T) {
	for _, seed := range []bool{true, false} {
		name := map[bool]string{true: "existing branch", false: "empty remote"}[seed]
		t.Run(name, func(t *testing.T) {
			remote := newRemote(t, seed)
			r := open(t, remote)
			res, err := r.Update(context.Background(), addFile("transcripts/unsorted/a.md", "a\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !res.Pushed || res.Commit == "" {
				t.Fatalf("result = %+v", res)
			}
			if got := run(t, "", "git", "--git-dir", remote, "rev-parse", "main"); got != res.Commit {
				t.Errorf("remote main = %s, want %s", got, res.Commit)
			}
			if got := run(t, "", "git", "--git-dir", remote, "show", "main:transcripts/unsorted/a.md"); got != "a" {
				t.Errorf("remote file = %q", got)
			}
		})
	}
}

func TestUpdateRebasesOnConcurrentPush(t *testing.T) {
	remote := newRemote(t, true)
	r := open(t, remote)

	// Someone else (Antonio's laptop) pushes after the server cloned.
	other := filepath.Join(t.TempDir(), "other")
	run(t, "", "git", "clone", "--quiet", remote, other)
	writeFile(t, other, "notes.md", "from laptop\n")
	commitAll(t, other, "laptop edit")
	run(t, other, "git", "push", "--quiet", "origin", "HEAD:main")

	if _, err := r.Update(context.Background(), addFile("b.md", "b\n")); err != nil {
		t.Fatal(err)
	}
	log := run(t, "", "git", "--git-dir", remote, "log", "--format=%s", "main")
	if log != "add b.md\nlaptop edit\nseed" {
		t.Errorf("remote history:\n%s", log)
	}
}

func TestPushFailureKeepsCommitAndSyncRecovers(t *testing.T) {
	remote := newRemote(t, true)
	r := open(t, remote)

	moved := remote + ".away"
	if err := os.Rename(remote, moved); err != nil {
		t.Fatal(err)
	}
	res, err := r.Update(context.Background(), addFile("c.md", "c\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed || res.PushErr == nil || res.Commit == "" {
		t.Fatalf("want local commit with push error, got %+v", res)
	}

	if err := os.Rename(moved, remote); err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := run(t, "", "git", "--git-dir", remote, "rev-parse", "main"); got != res.Commit {
		t.Errorf("after sync remote main = %s, want %s", got, res.Commit)
	}
}

func TestFailedUpdateRevertsFiles(t *testing.T) {
	r := open(t, newRemote(t, true))
	_, err := r.Update(context.Background(), func(root string) (Change, error) {
		ch, _ := addFile("d.md", "d\n")(root)
		return ch, errors.New("boom")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(filepath.Join(r.Dir(), "d.md")); !os.IsNotExist(err) {
		t.Error("d.md should have been removed")
	}
	if s := run(t, r.Dir(), "git", "status", "--porcelain"); s != "" {
		t.Errorf("working copy dirty: %s", s)
	}
}

func TestOpenStashesLeftoversAndRejectsOtherRemote(t *testing.T) {
	remote := newRemote(t, true)
	dir := filepath.Join(t.TempDir(), "work")
	opts := Options{RemoteURL: remote, Branch: "main", Dir: dir, AuthorName: "t", AuthorEmail: "t@e"}
	if _, err := Open(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "leftover.md", "x\n")
	if _, err := Open(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if s := run(t, dir, "git", "status", "--porcelain"); s != "" {
		t.Errorf("leftover not stashed: %s", s)
	}
	if !strings.Contains(run(t, dir, "git", "stash", "list"), "uncommitted changes found at startup") {
		t.Error("stash entry missing")
	}

	opts.RemoteURL = newRemote(t, false)
	if _, err := Open(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "is a clone of") {
		t.Errorf("want remote mismatch error, got %v", err)
	}
}

func TestGitEnvQuotesPaths(t *testing.T) {
	env := gitEnv(Options{SSHKeyPath: "/keys/it's key", KnownHostsPath: "/kh"})
	var ssh string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GIT_SSH_COMMAND="); ok {
			ssh = v
		}
	}
	want := `ssh -o BatchMode=yes -i '/keys/it'\''s key' -o IdentitiesOnly=yes -o UserKnownHostsFile='/kh' -o StrictHostKeyChecking=yes`
	if ssh != want {
		t.Errorf("GIT_SSH_COMMAND = %s", ssh)
	}
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", msg)
}
