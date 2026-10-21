package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"soundwave/pkg/events"
)

func TestHandleTrackPlayed_MalformedDataReturnsError(t *testing.T) {
	for _, data := range []string{`"just a string"`, `[1,2]`, `{"track_id":5}`} {
		env := events.Envelope{ID: "e1", Type: events.TrackPlayed, Data: json.RawMessage(data)}
		if err := handleTrackPlayed(env); err == nil {
			t.Errorf("data %s: expected an error, got nil", data)
		}
	}
}

func TestMilestoneDefaultIsFive(t *testing.T) {
	if milestoneEvery != 5 {
		t.Errorf("milestoneEvery = %d, want 5 (documented behaviour)", milestoneEvery)
	}
}

// Boundary values around the milestone rule used in handleTrackPlayed (count % milestoneEvery == 0).
func TestMilestoneRuleBoundaries(t *testing.T) {
	cases := map[int]bool{1: false, 4: false, 5: true, 6: false, 9: false, 10: true, 11: false}
	for count, want := range cases {
		if got := count%milestoneEvery == 0; got != want {
			t.Errorf("count %d: milestone = %v, want %v", count, got, want)
		}
	}
}

func TestWithCORS_Preflight(t *testing.T) {
	rec := httptest.NewRecorder()
	withCORS(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/stats/top", nil))
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: status %d headers %v", rec.Code, rec.Header())
	}
}
