package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Equivalence classes for POST /plays input. Every invalid class must be
// rejected with 400 BEFORE the database or broker are touched, so no
// infrastructure is needed here.
func TestCreatePlay_RejectsInvalidInput(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed JSON", `{bad json`},
		{"empty body", ``},
		{"empty object", `{}`},
		{"missing user_id", `{"track_id":"t1","title":"S"}`},
		{"missing track_id", `{"user_id":"u1","title":"S"}`},
		{"missing title", `{"user_id":"u1","track_id":"t1"}`},
		{"empty user_id", `{"user_id":"","track_id":"t1","title":"S"}`},
		{"empty title", `{"user_id":"u1","track_id":"t1","title":""}`},
		{"JSON array instead of object", `[1,2,3]`},
		{"wrong field type", `{"user_id":5,"track_id":"t1","title":"S"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/plays", strings.NewReader(c.body))
			createPlay(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var e map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
				t.Errorf("want a JSON body with an error message, got %q", rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

func TestWithCORS_PreflightReturns204WithHeaders(t *testing.T) {
	nextCalled := false
	h := withCORS(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/plays", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if nextCalled {
		t.Error("preflight must not reach the real handler")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("missing CORS headers: %v", rec.Header())
	}
}

func TestWithCORS_NormalRequestPassesThroughWithHeader(t *testing.T) {
	h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want the wrapped handler's 418", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header missing on a normal response")
	}
}
