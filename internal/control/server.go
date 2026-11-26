// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package control

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"

	"github.com/HakeemTheEmperor/pentymento/internal/agent"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Paths of the control API (SDD 9.3).
const (
	PathStatus  = "/v1/status"
	PathReload  = "/v1/reload"
	PathBackup  = "/v1/backup"
	PathHealth  = "/health"
	PathReady   = "/ready"
	PathMetrics = "/metrics"
)

// Agent is what the server reports on.
type Agent interface {
	Status() agent.Status
	Ready() bool
}

// Server serves the control API on the control socket.
type Server struct {
	Agent Agent
	// Reload reloads the configuration.
	Reload func(ctx context.Context) (*agent.ReloadResult, error)
	// Backup takes a backup of a target through the agent now, returning
	// what to report.
	Backup func(ctx context.Context, target string) (any, error)
	// Metrics serves the Prometheus metrics.
	Metrics http.Handler
	// PProf serves Go's profiling endpoints (agent.debug.pprof).
	PProf  bool
	Logger *slog.Logger
}

// Handler is the full control API, for the local socket only.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	addHealth(mux, s.Agent, s.Metrics)
	mux.HandleFunc("GET "+PathStatus, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Agent.Status())
	})
	mux.HandleFunc("POST "+PathReload, func(w http.ResponseWriter, r *http.Request) {
		res, err := s.Reload(r.Context())
		s.reply(w, res, err)
	})
	mux.HandleFunc("POST "+PathBackup, func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			s.reply(w, nil, &msg.Error{Code: msg.ExitUsage, Message: msg.Message{Level: msg.LevelFailed, Fact: "no target named"}})
			return
		}
		res, err := s.Backup(r.Context(), target)
		s.reply(w, res, err)
	})
	if s.PProf {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// HealthHandler serves only /health, /ready and /metrics: the opt-in TCP
// health port, where no control action is ever reachable.
func HealthHandler(a Agent, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	addHealth(mux, a, metrics)
	return mux
}

func addHealth(mux *http.ServeMux, a Agent, metrics http.Handler) {
	mux.HandleFunc("GET "+PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET "+PathReady, func(w http.ResponseWriter, _ *http.Request) {
		if a.Ready() {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
	})
	if metrics != nil {
		mux.Handle("GET "+PathMetrics, metrics)
	}
}

// ErrorBody is an error over the control API: the message the CLI shows
// and the exit code it exits with.
type ErrorBody struct {
	Code    int    `json:"code"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
	Cause   string `json:"cause,omitempty"`
	Impact  string `json:"impact,omitempty"`
	Next    string `json:"next,omitempty"`
}

func (s *Server) reply(w http.ResponseWriter, v any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	body := ErrorBody{Code: int(msg.ExitFailure), Message: secrets.Scrub(err.Error())}
	var me *msg.Error
	if errors.As(err, &me) {
		body = ErrorBody{Code: int(me.Code), ID: string(me.Message.ID), Message: me.Message.Fact,
			Cause: secrets.Scrub(me.Message.Cause), Impact: me.Message.Impact, Next: me.Message.Next}
	}
	if s.Logger != nil {
		s.Logger.Warn("a control request failed", "error", body.Message)
	}
	status := http.StatusInternalServerError
	switch msg.ExitCode(body.Code) {
	case msg.ExitUsage, msg.ExitConfig:
		status = http.StatusBadRequest
	case msg.ExitNotFound:
		status = http.StatusNotFound
	}
	writeJSON(w, status, struct {
		Error ErrorBody `json:"error"`
	}{body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
