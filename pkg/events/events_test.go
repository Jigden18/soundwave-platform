package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Contract test: other services and the UI rely on these exact names.
func TestRoutingKeysAreStable(t *testing.T) {
	cases := map[string]string{
		TrackPlayed:           "track.played",
		TrackMilestoneReached: "track.milestone_reached",
		PlaylistCreated:       "playlist.created",
		ExchangeName:          "soundwave.events",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("constant changed: got %q, want %q", got, want)
		}
	}
}

func TestNewEnvelope_SetsTypeAndKeepsData(t *testing.T) {
	in := TrackPlayedData{UserID: "u1", TrackID: "t1", ArtistID: "a1", Title: "Song"}
	env, err := NewEnvelope(TrackPlayed, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.Type != TrackPlayed {
		t.Errorf("type = %q, want %q", env.Type, TrackPlayed)
	}
	var out TrackPlayedData
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatalf("data is not valid JSON: %v", err)
	}
	if out != in {
		t.Errorf("data round trip = %+v, want %+v", out, in)
	}
}

// Duplicate protection depends on every event having its own id.
func TestNewEnvelope_GeneratesUniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		env, _ := NewEnvelope(PlaylistCreated, PlaylistCreatedData{Name: "x"})
		if env.ID == "" {
			t.Fatal("empty event id")
		}
		if seen[env.ID] {
			t.Fatalf("duplicate event id %s", env.ID)
		}
		seen[env.ID] = true
	}
}

func TestNewEnvelope_TimestampIsRecentUTC(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	env, _ := NewEnvelope(TrackPlayed, TrackPlayedData{})
	if env.Timestamp.Location() != time.UTC {
		t.Errorf("timestamp not UTC: %v", env.Timestamp.Location())
	}
	if env.Timestamp.Before(before) || env.Timestamp.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("timestamp not close to now: %v", env.Timestamp)
	}
}

func TestNewEnvelope_UnserialisableDataReturnsError(t *testing.T) {
	if _, err := NewEnvelope(TrackPlayed, make(chan int)); err == nil {
		t.Error("expected an error for data that cannot be marshalled")
	}
}

// The JSON field names are part of the documented event format.
func TestEnvelope_JSONFieldNames(t *testing.T) {
	env, _ := NewEnvelope(TrackMilestoneReached, TrackMilestoneData{TrackID: "t1", Title: "S", PlayCount: 5})
	raw, _ := json.Marshal(env)
	for _, f := range []string{`"event_id"`, `"event_type"`, `"timestamp"`, `"data"`, `"play_count":5`, `"track_id":"t1"`} {
		if !strings.Contains(string(raw), f) {
			t.Errorf("serialised event is missing %s: %s", f, raw)
		}
	}
}

func TestEnvelope_RoundTripThroughJSON(t *testing.T) {
	env, _ := NewEnvelope(PlaylistCreated, PlaylistCreatedData{PlaylistID: "p1", UserID: "u1", Name: "Mix"})
	raw, _ := json.Marshal(env)
	var back Envelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != env.ID || back.Type != env.Type {
		t.Errorf("id/type changed: %+v vs %+v", back, env)
	}
	var d PlaylistCreatedData
	_ = json.Unmarshal(back.Data, &d)
	if d.Name != "Mix" || d.PlaylistID != "p1" || d.UserID != "u1" {
		t.Errorf("payload changed: %+v", d)
	}
}
