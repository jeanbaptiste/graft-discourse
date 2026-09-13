// Package admin exposes a small authenticated HTTP API for managing
// topic<->issue bindings at runtime, so operators don't have to edit the
// config file and restart to link a new topic.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"graftdiscourse/internal/graft"
)

// Binder is the subset of the bridge the admin API drives.
type Binder interface {
	Bind(ctx context.Context, topicID int64, noteURI string) error
	Unbind(topicID int64) error
	Mappings() map[int64]string
	Issues(ctx context.Context) ([]graft.IssueRef, error)
}

// Server is the admin API handler. Token is the required bearer token; with
// an empty token the routes are not registered (see Register).
type Server struct {
	Token  string
	Binder Binder
	Log    *slog.Logger
}

const maxBody = 4 << 10

type bindRequest struct {
	TopicID int64  `json:"topic_id"`
	NoteURI string `json:"note_uri"`
}

// Register mounts the admin routes on mux. It is a no-op when no token is
// configured, so the API cannot be exposed unauthenticated by accident.
func (s *Server) Register(mux *http.ServeMux) {
	if s.Token == "" {
		return
	}
	mux.HandleFunc("/admin/mappings", s.auth(s.handleMappings))
	mux.HandleFunc("/admin/mappings/", s.auth(s.handleMapping))
	mux.HandleFunc("/admin/issues", s.auth(s.handleIssues))
}

// auth enforces a bearer token in constant time.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// s.Token == "" is already unreachable via Register (it doesn't
		// mount these routes), but auth doesn't rely on that: an empty
		// bearer token must never authenticate here even if some future
		// caller wires this handler up directly.
		if s.Token == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleMappings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.Binder.Mappings())
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		var req bindRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.TopicID <= 0 || req.NoteURI == "" {
			http.Error(w, "topic_id and note_uri are required", http.StatusBadRequest)
			return
		}
		if err := s.Binder.Bind(r.Context(), req.TopicID, req.NoteURI); err != nil {
			s.logErr("bind failed", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, req)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

// handleIssues lists replyable Graft issues/patches, optionally filtered by
// ?series= and ?q= (case-insensitive substring on the title).
func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	issues, err := s.Binder.Issues(r.Context())
	if err != nil {
		s.logErr("list issues failed", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	series := r.URL.Query().Get("series")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := make([]graft.IssueRef, 0, len(issues))
	for _, it := range issues {
		if series != "" && it.Series != series {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(it.Title), q) {
			continue
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, "DELETE")
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/admin/mappings/")
	topicID, err := strconv.ParseInt(strings.Trim(raw, "/"), 10, 64)
	if err != nil || topicID <= 0 {
		http.Error(w, "invalid topic id", http.StatusBadRequest)
		return
	}
	if err := s.Binder.Unbind(topicID); err != nil {
		s.logErr("unbind failed", err)
		http.Error(w, "unbind failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) logErr(msg string, err error) {
	if s.Log != nil {
		s.Log.Error(msg, "err", err)
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		// Response is already committed; nothing more to do.
		_ = err
	}
}
