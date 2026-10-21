package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"soundwave/pkg/events"
)

func TestCreatePlaylist_RejectsInvalidInput(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed JSON", `{bad`},
		{"empty body", ``},
		{"empty object", `{}`},
		{"missing user_id", `{"name":"Mix"}`},
		{"missing name", `{"user_id":"u1"}`},
		{"empty name", `{"user_id":"u1","name":""}`},
		{"empty user_id", `{"user_id":"","name":"Mix"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			createPlaylist(rec, httptest.NewRequest("POST", "/playlists", strings.NewReader(c.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var e map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
				t.Errorf("want JSON error body, got %q", rec.Body.String())
			}
		})
	}
}

// A consumer must return an error (so the broker retries / dead-letters)
// when the event data cannot be understood.
func TestHandleTrackPlayed_MalformedDataReturnsError(t *testing.T) {
	for _, data := range []string{`"just a string"`, `[1,2]`, `{"user_id":5}`} {
		env := events.Envelope{ID: "e1", Type: events.TrackPlayed, Data: json.RawMessage(data)}
		if err := handleTrackPlayed(env); err == nil {
			t.Errorf("data %s: expected an error, got nil", data)
		}
	}
}

func TestWithCORS_Preflight(t *testing.T) {
	rec := httptest.NewRecorder()
	withCORS(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/playlists", nil))
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: status %d headers %v", rec.Code, rec.Header())
	}
}
