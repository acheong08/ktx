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
	"runtime/debug"
	"syscall"
	"time"

	"github.com/acheong08/ktx/internal/ai"
	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/config"
	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/connectors/granola"
	"github.com/acheong08/ktx/internal/connectors/pocket"
	"github.com/acheong08/ktx/internal/dav"
	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/mcpserver"
	"github.com/acheong08/ktx/internal/notes"
	"github.com/acheong08/ktx/internal/oauth"
	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/server"
	"github.com/acheong08/ktx/internal/state"
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
	st.SetLinker(calendar.Linker{}) // link transcripts to the calendar event they were recorded in
	cal := calendar.NewService(repo, st.Relink)

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer db.Close()

	// The workspace's time zone (Settings → General), before anything reads
	// calendars or shows times.
	if name, err := db.TimeZone(); err != nil {
		return err
	} else if name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			calendar.SetDefaultZone(loc)
		} else {
			log.Warn("stored time zone is unknown; using the default", "zone", name, "err", err)
		}
	}
	log.Info("time zone", "zone", calendar.DefaultZone().String())

	// Credentials for connectors are sealed with the master key. Without one,
	// connections can't be created or run, but everything else works.
	var box *secrets.Box
	if path := secrets.KeyPath(cfg.MasterKeyFile); path != "" {
		if box, err = secrets.Load(path); err != nil {
			return fmt.Errorf("master key: %w", err)
		}
	} else {
		log.Warn("no master key configured; connectors are disabled (set BRAIN_MASTER_KEY_FILE or the systemd credential " + secrets.CredentialName + ")")
	}
	conns := connectors.NewManager(db, box, st, log, granola.New(), pocket.New())
	if err := conns.Start(ctx); err != nil {
		return err
	}

	var auth *web.Auth
	if cfg.OIDCIssuer != "" {
		auth = web.NewAuth(web.OIDCConfig{
			Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			PublicURL: cfg.PublicURL, AdminGroup: cfg.AdminGroup, SessionTTL: cfg.SessionTTL,
		}, db, log)
		log.Info("sign-in through OIDC", "issuer", cfg.OIDCIssuer, "admin_group", cfg.AdminGroup)
	} else {
		log.Warn("no sign-in configured (BRAIN_OIDC_ISSUER): the web UI is open to anyone who can reach it")
	}

	mux := http.NewServeMux()
	server.New(st, cfg.Tokens, db, cfg.MaxUploadBytes, log).Register(mux)
	dav.New(cal, db, log).Register(mux) // CalDAV, with calendar tokens
	go func() {                         // apply the current matching rules to every transcript
		if n, err := cal.RelinkAll(ctx); err != nil {
			log.Error("relinking transcripts", "err", err)
		} else if n > 0 {
			log.Info("relinked transcripts to calendar events", "changed", n)
		}
	}()
	var types []string
	for _, t := range cfg.Types {
		if t, err = transcript.NormalizeType(t); err != nil {
			return fmt.Errorf("BRAIN_TRANSCRIPT_TYPES: %w", err)
		}
		if t != "" {
			types = append(types, t)
		}
	}
	aiSettings := ai.NewSettings(db, box) // without a master key, notes get first-line titles
	nts := notes.New(repo, db, aiSettings, log)
	nts.Start(ctx) // names and commits notes in the background, including ones queued before a restart
	ui := web.Options{Store: st, Types: types, Log: log, DB: db, Auth: auth, Calendar: cal, Notes: nts}
	if auth != nil {
		ui.AI = aiSettings    // admins set the key
		ui.Connectors = conns // connections belong to people, so they need sign-in
		// MCP for agents: apps get tokens over OAuth, approved by a signed-in person.
		ui.OAuth = oauth.New(db, cfg.PublicURL, log)
		ui.OAuth.Register(mux)
		mcpserver.New(st, cal, nts, ui.OAuth, version(), log).Register(mux)
		log.Info("MCP server enabled", "url", ui.OAuth.ResourceURL())
	}
	mux.Handle("/", web.New(ui).Handler())

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

// version is the module version the binary was built from, if known.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
