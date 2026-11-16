// Command brain-server keeps a clone of the brain git repository and exposes
// an HTTP API that commits uploaded transcripts and pushes them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/acheong08/ktx/internal/config"
	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/server"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
	"github.com/acheong08/ktx/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	openCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	repo, err := gitrepo.Open(openCtx, gitrepo.Options{
		RemoteURL:      cfg.RepoURL,
		Branch:         cfg.Branch,
		Dir:            cfg.WorkDir,
		SSHKeyPath:     cfg.SSHKeyPath,
		KnownHostsPath: cfg.KnownHostsPath,
		AuthorName:     cfg.AuthorName,
		AuthorEmail:    cfg.AuthorEmail,
		Logger:         log,
	})
	cancel()
	if err != nil {
		return err
	}

	if cfg.SyncInterval > 0 {
		go func() {
			t := time.NewTicker(cfg.SyncInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					syncCtx, cancel := context.WithTimeout(ctx, time.Minute)
					if err := repo.Sync(syncCtx); err != nil {
						log.Warn("periodic sync failed", "err", err)
					}
					cancel()
				}
			}
		}()
	}

	st := store.New(repo)
	mux := http.NewServeMux()
	server.New(st, cfg.Tokens, cfg.MaxUploadBytes, log).Register(mux)
	if cfg.AuthorizedKeys != "" {
		keys, err := web.ParseAuthorizedKeys(cfg.AuthorizedKeys)
		if err != nil {
			return err
		}
		var types []string
		for _, t := range cfg.Types {
			if t, err = transcript.NormalizeType(t); err != nil {
				return fmt.Errorf("BRAIN_TRANSCRIPT_TYPES: %w", err)
			}
			if t != "" {
				types = append(types, t)
			}
		}
		for _, k := range keys {
			log.Info("UI login allowed", "user", k.Name, "key", ssh.FingerprintSHA256(k.Key))
		}
		mux.Handle("/", web.New(st, web.NewAuth(keys, cfg.SessionTTL), types, log).Handler())
	} else {
		log.Info("UI disabled: set BRAIN_UI_AUTHORIZED_KEYS or BRAIN_UI_AUTHORIZED_KEYS_FILE to enable it")
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      3 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "repo", cfg.RepoURL, "branch", cfg.Branch, "workdir", cfg.WorkDir)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
