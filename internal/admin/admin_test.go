package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"graftdiscourse/internal/graft"
)

type fakeBinder struct {
	mappings  map[int64]string
	binds     []string
	unbound   []int64
	bindErr   error
	issues    []graft.IssueRef
	issuesErr error
}

func (f *fakeBinder) Bind(_ context.Context, topicID int64, noteURI string) error {
	if f.bindErr != nil {
		return f.bindErr
	}
	f.binds = append(f.binds, fmt.Sprintf("%d=%s", topicID, noteURI))
	f.mappings[topicID] = noteURI
	return nil
}

func (f *fakeBinder) Unbind(topicID int64) error {
	f.unbound = append(f.unbound, topicID)
	delete(f.mappings, topicID)
	return nil
}

func (f *fakeBinder) Mappings() map[int64]string { return f.mappings }

func (f *fakeBinder) Issues(context.Context) ([]graft.IssueRef, error) {
	return f.issues, f.issuesErr
}

func newServer() (*Server, *fakeBinder) {
	fb := &fakeBinder{mappings: map[int64]string{}}
	return &Server{Token: "secret", Binder: fb}, fb
}

func do(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAdminRequiresToken(t *testing.T) {
	s, _ := newServer()
	if rec := do(t, s, http.MethodGet, "/admin/mappings", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/admin/mappings", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}
}

func TestAdminDisabledWithoutToken(t *testing.T) {
	s := &Server{Binder: &fakeBinder{mappings: map[int64]string{}}}
	if rec := do(t, s, http.MethodGet, "/admin/mappings", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled admin: got %d, want 404", rec.Code)
	}
}

func TestBindAndList(t *testing.T) {
	s, fb := newServer()
	rec := do(t, s, http.MethodPost, "/admin/mappings", "secret",
		`{"topic_id":100,"note_uri":"https://g.example/actors/x/notes/7"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bind: got %d, want 201: %s", rec.Code, rec.Body)
	}
	if len(fb.binds) != 1 {
		t.Fatalf("binder not called: %+v", fb.binds)
	}
	rec = do(t, s, http.MethodGet, "/admin/mappings", "secret", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "100") {
		t.Fatalf("list: got %d body %s", rec.Code, rec.Body)
	}
}

func TestBindRejectsBadInput(t *testing.T) {
	s, _ := newServer()
	if rec := do(t, s, http.MethodPost, "/admin/mappings", "secret", `{"topic_id":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields: got %d, want 400", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/admin/mappings", "secret", `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: got %d, want 400", rec.Code)
	}
}

func TestUnbind(t *testing.T) {
	s, fb := newServer()
	rec := do(t, s, http.MethodDelete, "/admin/mappings/100", "secret", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unbind: got %d, want 204", rec.Code)
	}
	if len(fb.unbound) != 1 || fb.unbound[0] != 100 {
		t.Fatalf("unbind not called correctly: %+v", fb.unbound)
	}
}

func TestListIssuesWithFilters(t *testing.T) {
	s, fb := newServer()
	fb.issues = []graft.IssueRef{
		{Series: "fedx", EntryID: 7, Title: "Fix flux capacitor", NoteURI: "https://g/actors/fedx/notes/7"},
		{Series: "fedx", EntryID: 8, Title: "Add time machine", NoteURI: "https://g/actors/fedx/notes/8"},
		{Series: "other", EntryID: 1, Title: "Flux meter", NoteURI: "https://g/actors/other/notes/1"},
	}

	rec := do(t, s, http.MethodGet, "/admin/issues?series=fedx&q=flux", "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("issues: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Fix flux capacitor") || strings.Contains(body, "Add time machine") || strings.Contains(body, "Flux meter") {
		t.Fatalf("filter wrong: %s", body)
	}

	rec = do(t, s, http.MethodPost, "/admin/issues", "secret", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST issues: got %d, want 405", rec.Code)
	}
}
