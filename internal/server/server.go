// Package server exposes the JSON API used by ingestion clients.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

// Server handles API requests.
type Server struct {
	store    *store.Store
	tokens   map[string]string // bearer token -> user name
	maxBytes int64
	log      *slog.Logger
}

// New builds a Server. tokens maps each bearer token to the name of its user.
func New(st *store.Store, tokens map[string]string, maxBytes int64, log *slog.Logger) *Server {
	return &Server{store: st, tokens: tokens, maxBytes: maxBytes, log: log}
}

// Register adds the API routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v1/transcripts", s.auth(s.createTranscript))
}

type ctxUser struct{}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok {
			for token, user := range s.tokens {
				if subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
					next(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
					return
				}
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="brain"`)
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token", nil)
	}
}

// TranscriptResponse is returned by POST /v1/transcripts.
type TranscriptResponse struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"` // set when an update moved the file
	Result       string `json:"result"`                  // created, updated or unchanged
	Commit       string `json:"commit,omitempty"`
	Pushed       bool   `json:"pushed"`
	PushError    string `json:"push_error,omitempty"`
}

func (s *Server) createTranscript(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(ctxUser{}).(string)

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("transcript is larger than %d bytes", s.maxBytes), nil)
			return
		}
		writeError(w, http.StatusBadRequest, "could not read request body", nil)
		return
	}

	doc, err := transcript.Parse(raw)
	if err != nil {
		var verr *transcript.ValidationError
		if errors.As(err, &verr) {
			writeError(w, http.StatusBadRequest, "invalid transcript", verr.Problems)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// Don't let a client disconnect interrupt git halfway through a commit or push.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	res, err := s.store.Put(ctx, doc, user)
	if err != nil {
		s.log.Error("storing transcript failed", "id", doc.ID, "user", user, "err", err)
		writeError(w, http.StatusInternalServerError, "could not store transcript", nil)
		return
	}

	resp := TranscriptResponse{
		ID: doc.ID, Path: res.Path, PreviousPath: res.PreviousPath, Result: res.Result,
		Commit: res.Git.Commit, Pushed: res.Git.Pushed,
	}
	if res.Git.PushErr != nil {
		resp.PushError = "saved and committed on the server, but not pushed yet; it will be pushed on the next sync"
	}
	status := http.StatusOK
	if res.Result == store.ResultCreated {
		status = http.StatusCreated
	}
	if res.Result != store.ResultUnchanged {
		s.log.Info("transcript stored", "result", res.Result, "id", doc.ID, "path", res.Path,
			"previous_path", res.PreviousPath, "user", user, "commit", res.Git.Commit, "pushed", res.Git.Pushed)
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, details []string) {
	body := map[string]any{"error": msg}
	if len(details) > 0 {
		body["details"] = details
	}
	writeJSON(w, status, body)
}
