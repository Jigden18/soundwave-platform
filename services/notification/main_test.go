package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"soundwave/pkg/events"
)

func TestHandleEvent_UnknownTypeIsIgnoredWithoutError(t *testing.T) {
	env := events.Envelope{ID: "e1", Type: "something.else", Data: json.RawMessage(`{}`)}
	if err := handleEvent(env); err != nil {
		t.Errorf("unknown event types must be acknowledged, got error %v", err)
	}
}

func TestHandleEvent_MalformedDataReturnsError(t *testing.T) {
	cases := []struct{ typ, data string }{
		{events.TrackMilestoneReached, `"text"`},
		{events.TrackMilestoneReached, `{"play_count":"five"}`},
		{events.PlaylistCreated, `[1]`},
		{events.PlaylistCreated, `{"name":7}`},
	}
	for _, c := range cases {
		env := events.Envelope{ID: "e1", Type: c.typ, Data: json.RawMessage(c.data)}
		if err := handleEvent(env); err == nil {
			t.Errorf("%s with %s: expected an error so the broker retries", c.typ, c.data)
		}
	}
}

// The poison-message switch used in the dead-letter demo.
func TestHandleEvent_SimulatedFailure(t *testing.T) {
	t.Setenv("SIMULATE_FAILURE_FOR", "Poison")
	data, _ := json.Marshal(events.PlaylistCreatedData{PlaylistID: "p1", UserID: "u1", Name: "Poison Playlist"})
	err := handleEvent(events.Envelope{ID: "e1", Type: events.PlaylistCreated, Data: data})
	if err == nil || !strings.Contains(err.Error(), "simulated") {
		t.Errorf("expected the simulated failure, got %v", err)
	}
	data, _ = json.Marshal(events.TrackMilestoneData{TrackID: "t1", Title: "Poison Song", PlayCount: 5})
	err = handleEvent(events.Envelope{ID: "e2", Type: events.TrackMilestoneReached, Data: data})
	if err == nil || !strings.Contains(err.Error(), "simulated") {
		t.Errorf("expected the simulated failure for milestone events too, got %v", err)
	}
}

func TestWithCORS_Preflight(t *testing.T) {
	rec := httptest.NewRecorder()
	withCORS(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/notifications", nil))
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: status %d headers %v", rec.Code, rec.Header())
	}
}
