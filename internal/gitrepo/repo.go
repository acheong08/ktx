// Package gitrepo manages the server's working copy of the brain repository.
// It shells out to the git binary. Every operation holds one lock, so the
// server is the only writer to its working copy.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options configure a Repo.
type Options struct {
	RemoteURL      string // e.g. git@github.com:melzi/brain.git
	Branch         string // e.g. main
	Dir            string // working copy location; cloned if missing
	SSHKeyPath     string // deploy key; empty uses the default ssh setup
	KnownHostsPath string // pinned known_hosts; empty accepts a new host key on first use
	AuthorName     string
	AuthorEmail    string
	Logger         *slog.Logger
}

// Repo is a working copy that pulls before and pushes after each change.
type Repo struct {
	opt Options
	env []string
	log *slog.Logger
	mu  sync.Mutex
}

// Change is what an update function wants committed.
type Change struct {
	Paths   []string // paths relative to the repo root; empty means commit nothing
	Message string
}

// Result reports what happened to a change.
type Result struct {
	Commit  string // HEAD after the commit; empty if nothing was committed
	Pushed  bool
	PushErr error // set when the commit is saved locally but not yet on the remote
}

// Open clones the repository if needed, checks the existing clone points at
// the configured remote, sets aside leftovers from a crash, and syncs.
func Open(ctx context.Context, opt Options) (*Repo, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	r := &Repo{opt: opt, log: opt.Logger, env: gitEnv(opt)}

	if _, err := os.Stat(filepath.Join(opt.Dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := r.clone(ctx); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		url, err := r.git(ctx, "remote", "get-url", "origin")
		if err != nil {
			return nil, err
		}
		if url != opt.RemoteURL {
			return nil, fmt.Errorf("%s is a clone of %q, not %q; move it or change BRAIN_WORKDIR", opt.Dir, url, opt.RemoteURL)
		}
	}

	if status, err := r.git(ctx, "status", "--porcelain"); err != nil {
		return nil, err
	} else if status != "" {
		r.log.Warn("working copy has uncommitted changes; stashing them", "status", status)
		msg := "brain-server: uncommitted changes found at startup " + time.Now().UTC().Format(time.RFC3339)
		if _, err := r.git(ctx, "stash", "push", "--include-untracked", "-m", msg); err != nil {
			return nil, err
		}
	}

	if err := r.Sync(ctx); err != nil {
		r.log.Warn("initial sync failed; continuing with the local copy", "err", err)
	}
	return r, nil
}

// Dir is the working copy root.
func (r *Repo) Dir() string { return r.opt.Dir }

func (r *Repo) clone(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(r.opt.Dir), 0o755); err != nil {
		return err
	}
	r.log.Info("cloning", "remote", r.opt.RemoteURL, "dir", r.opt.Dir)
	if _, err := r.gitIn(ctx, "", "clone", "--quiet", r.opt.RemoteURL, r.opt.Dir); err != nil {
		return err
	}
	if r.hasRemoteBranch(ctx) {
		_, err := r.git(ctx, "checkout", "-B", r.opt.Branch, "origin/"+r.opt.Branch)
		return err
	}
	// Empty repository, or the branch does not exist yet: the first push creates it.
	if _, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		_, err := r.git(ctx, "checkout", "-b", r.opt.Branch)
		return err
	}
	_, err := r.git(ctx, "symbolic-ref", "HEAD", "refs/heads/"+r.opt.Branch)
	return err
}

// Update refreshes from the remote, runs fn with the working copy root while
// holding the lock, then commits fn's paths and pushes. If the remote is
// unreachable the commit stays local and the next Update or Sync pushes it;
// Result.PushErr says so. If fn or the commit fails, fn's paths are reverted.
func (r *Repo) Update(ctx context.Context, fn func(root string) (Change, error)) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.pullRebase(ctx); err != nil {
		r.log.Warn("pull before update failed; committing on the local copy", "err", err)
	}

	ch, err := fn(r.opt.Dir)
	if err != nil {
		r.revert(ctx, ch.Paths)
		return Result{}, err
	}
	if len(ch.Paths) == 0 {
		return Result{}, nil
	}

	if _, err := r.git(ctx, append([]string{"add", "--"}, ch.Paths...)...); err != nil {
		r.revert(ctx, ch.Paths)
		return Result{}, err
	}
	if _, err := r.git(ctx,
		"-c", "user.name="+r.opt.AuthorName,
		"-c", "user.email="+r.opt.AuthorEmail,
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", ch.Message,
	); err != nil {
		r.revert(ctx, ch.Paths)
		return Result{}, err
	}
	head, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}

	res := Result{Commit: head}
	if err := r.pushWithRetry(ctx); err != nil {
		r.log.Warn("push failed; commit kept locally", "commit", head, "err", err)
		res.PushErr = err
	} else {
		res.Pushed = true
	}
	return res, nil
}

// View runs fn with the working copy root while holding the lock, so it never
// sees a half-applied pull or update. fn must not modify the working copy.
func (r *Repo) View(fn func(root string) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn(r.opt.Dir)
}

// Sync pulls remote changes and pushes any local commits that were not pushed.
func (r *Repo) Sync(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.pullRebase(ctx); err != nil {
		return err
	}
	if _, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return nil // nothing committed yet
	}
	if ahead, err := r.aheadOfRemote(ctx); err != nil || !ahead {
		return err
	}
	return r.pushWithRetry(ctx)
}

func (r *Repo) pullRebase(ctx context.Context) error {
	if _, err := r.git(ctx, "fetch", "--quiet", "origin"); err != nil {
		return err
	}
	if !r.hasRemoteBranch(ctx) {
		return nil
	}
	if _, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		// Unborn local branch: start from the remote one.
		_, err := r.git(ctx, "reset", "--quiet", "--hard", "origin/"+r.opt.Branch)
		return err
	}
	if _, err := r.git(ctx, "-c", "user.name="+r.opt.AuthorName, "-c", "user.email="+r.opt.AuthorEmail,
		"rebase", "--quiet", "origin/"+r.opt.Branch); err != nil {
		_, _ = r.git(ctx, "rebase", "--abort")
		return fmt.Errorf("local commits conflict with %s; resolve in %s by hand: %w", r.opt.Branch, r.opt.Dir, err)
	}
	return nil
}

func (r *Repo) pushWithRetry(ctx context.Context) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err = r.git(ctx, "push", "--quiet", "origin", "HEAD:refs/heads/"+r.opt.Branch); err == nil {
			return nil
		}
		// Most often someone pushed in between: rebase onto it and try again.
		if perr := r.pullRebase(ctx); perr != nil {
			return errors.Join(err, perr)
		}
	}
	return err
}

func (r *Repo) aheadOfRemote(ctx context.Context) (bool, error) {
	if !r.hasRemoteBranch(ctx) {
		return true, nil
	}
	n, err := r.git(ctx, "rev-list", "--count", "origin/"+r.opt.Branch+"..HEAD")
	return n != "0", err
}

func (r *Repo) hasRemoteBranch(ctx context.Context) bool {
	_, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+r.opt.Branch)
	return err == nil
}

// revert discards uncommitted changes to paths written by a failed update.
func (r *Repo) revert(ctx context.Context, paths []string) {
	for _, p := range paths {
		_, _ = r.git(ctx, "reset", "--quiet", "--", p)
		if _, err := r.git(ctx, "checkout", "--", p); err != nil {
			_ = os.Remove(filepath.Join(r.opt.Dir, p)) // untracked new file
		}
	}
}

func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	return r.gitIn(ctx, r.opt.Dir, args...)
}

func (r *Repo) gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gitEnv(opt Options) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if opt.SSHKeyPath == "" && opt.KnownHostsPath == "" {
		return env
	}
	ssh := []string{"ssh", "-o", "BatchMode=yes"}
	if opt.SSHKeyPath != "" {
		ssh = append(ssh, "-i", shellQuote(opt.SSHKeyPath), "-o", "IdentitiesOnly=yes")
	}
	if opt.KnownHostsPath != "" {
		ssh = append(ssh, "-o", "UserKnownHostsFile="+shellQuote(opt.KnownHostsPath), "-o", "StrictHostKeyChecking=yes")
	} else {
		ssh = append(ssh, "-o", "StrictHostKeyChecking=accept-new")
	}
	return append(env, "GIT_SSH_COMMAND="+strings.Join(ssh, " "))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
