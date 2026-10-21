// Package server implements the plugin's HTTP sidecar: the OpenAI
// compatible surface llm-proxy forwards to, plus the plugin protocol's
// management endpoints.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/chat"
	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/config"
)

const (
	// maxBodyBytes caps a request body.
	maxBodyBytes = 10 << 20
	// authTTL is how long a successful login check is trusted.
	authTTL = 60 * time.Second
)

// Driver is the part of claudecli.Driver the server uses.
type Driver interface {
	AuthStatus(ctx context.Context) (claudecli.AuthStatus, error)
	Catalogue(ctx context.Context) ([]claudecli.Model, error)
	Chat(ctx context.Context, req claudecli.Request, emit func(claudecli.Event) error) (claudecli.Result, error)
}

// Server serves the sidecar HTTP API.
type Server struct {
	cfg    config.Config
	driver Driver
	logger *slog.Logger
	now    func() time.Time
	slots  chan struct{}

	mu       sync.Mutex
	authedAt time.Time
	models   []string
}

// New returns a Server. A nil logger uses slog.Default.
func New(cfg config.Config, driver Driver, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:    cfg,
		driver: driver,
		logger: logger,
		now:    time.Now,
		slots:  make(chan struct{}, cfg.MaxConcurrency),
	}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /management/reload", s.handleReload)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	for _, p := range []string{"/v1/completions", "/v1/embeddings", "/v1/responses"} {
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, _ *http.Request) {
			chat.WriteError(w, &chat.Error{Status: http.StatusNotImplemented, Type: "not_implemented",
				Msg: "the claudecode backend supports only /v1/chat/completions"})
		})
	}
	return mux
}

// handleReload clears the cached login check and model catalogue so the
// next request sees a new login or plan change.
func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.authedAt = time.Time{}
	s.models = nil
	s.mu.Unlock()
	_, _ = io.WriteString(w, "reloaded\n")
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels lists the allow-list, or the account catalogue. It always
// answers 200: llm-proxy fails /v1/models for every backend when one
// backend errors, so a catalogue failure is logged and yields an empty
// list.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ids, err := s.modelIDs(r.Context())
	if err != nil {
		s.logger.Warn("list models", "backend", s.cfg.Name, "error", err)
	}
	data := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		data = append(data, modelEntry{ID: id, Object: "model", OwnedBy: "anthropic"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// modelIDs returns the IDs clients may use.
func (s *Server) modelIDs(ctx context.Context) ([]string, error) {
	if len(s.cfg.Models) > 0 {
		return s.cfg.Models, nil
	}
	s.mu.Lock()
	cached := s.models
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	models, err := s.driver.Catalogue(ctx)
	if err != nil {
		return nil, err
	}
	ids := claudecli.ModelIDs(models)
	s.mu.Lock()
	s.models = ids
	s.mu.Unlock()
	return ids, nil
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		chat.WriteError(w, &chat.Error{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error",
			Msg: fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)})
		return
	}
	req, err := chat.Decode(body)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	turn, err := chat.Translate(req, s.cfg.Effort)
	if err != nil {
		s.writeErr(w, err)
		return
	}

	// Wait for a free slot; each turn runs one claude process.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return
	}

	// Nothing is sent to the model until the login and the model are known
	// to be good.
	if err := s.checkAuth(ctx); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := s.checkModel(ctx, req.Model); err != nil {
		s.writeErr(w, err)
		return
	}

	id, created := chat.NewID(), s.now().Unix()
	if req.Stream {
		s.streamChat(w, r, turn, req, id, created)
		return
	}
	var acc chat.Accumulator
	res, err := s.driver.Chat(ctx, turn, func(ev claudecli.Event) error {
		acc.Add(ev)
		return nil
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acc.Completion(id, req.Model, created, res))
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, turn claudecli.Request, req *chat.Request, id string, created int64) {
	rc := http.NewResponseController(w)
	flush := func() { _ = rc.Flush() }
	st := chat.NewStreamer(w, flush, id, req.Model, created)
	begin := func() {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}
	res, err := s.driver.Chat(r.Context(), turn, func(ev claudecli.Event) error {
		if !st.Started() {
			begin()
		}
		return st.Event(ev)
	})
	if err != nil {
		if !st.Started() {
			s.writeErr(w, err)
			return
		}
		_ = st.Fail(s.classify(err))
		return
	}
	if !st.Started() {
		begin()
	}
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	if err := st.Finish(res, includeUsage); err != nil {
		s.logger.Debug("stream finish", "error", err)
	}
}

// checkAuth fails unless the CLI reports a login. A good result is cached
// for authTTL; a bad one is never cached.
func (s *Server) checkAuth(ctx context.Context) error {
	s.mu.Lock()
	fresh := !s.authedAt.IsZero() && s.now().Sub(s.authedAt) < authTTL
	s.mu.Unlock()
	if fresh {
		return nil
	}
	st, err := s.driver.AuthStatus(ctx)
	if err != nil {
		return err
	}
	if !st.LoggedIn {
		return &chat.Error{Status: http.StatusUnauthorized, Type: "authentication_error",
			Msg: fmt.Sprintf("the %s backend is not logged in to Claude Code; run `llm-proxy plugin run %s login`", s.cfg.Name, s.cfg.Name)}
	}
	s.mu.Lock()
	s.authedAt = s.now()
	s.mu.Unlock()
	return nil
}

func (s *Server) checkModel(ctx context.Context, model string) error {
	ids, err := s.modelIDs(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(ids, model) {
		return &chat.Error{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "model_not_found",
			Msg: fmt.Sprintf("model %q is not available on the %s backend", model, s.cfg.Name)}
	}
	return nil
}

// classify maps an error to an OpenAI error.
func (s *Server) classify(err error) *chat.Error {
	var ce *chat.Error
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, claudecli.ErrCLINotFound) {
		return &chat.Error{Status: http.StatusServiceUnavailable, Type: "server_error", Msg: err.Error()}
	}
	var ue *claudecli.UpstreamError
	if errors.As(err, &ue) {
		status := http.StatusBadGateway
		switch ue.Code {
		case "authentication_failed":
			status = http.StatusUnauthorized
		case "rate_limit", "billing_error":
			status = http.StatusTooManyRequests
		case "invalid_request":
			status = http.StatusBadRequest
		}
		return &chat.Error{Status: status, Type: "upstream_error", Msg: ue.Error()}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &chat.Error{Status: http.StatusGatewayTimeout, Type: "server_error", Msg: "request cancelled"}
	}
	s.logger.Error("chat request failed", "backend", s.cfg.Name, "error", err)
	return &chat.Error{Status: http.StatusInternalServerError, Type: "server_error", Msg: "internal error"}
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	chat.WriteError(w, s.classify(err))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
