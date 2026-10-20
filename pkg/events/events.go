package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const (
	ExchangeName    = "soundwave.events"
	DLXExchangeName = "soundwave.events.dlx"
)

// Routing keys / event types
const (
	TrackPlayed           = "track.played"
	TrackMilestoneReached = "track.milestone_reached"
	PlaylistCreated       = "playlist.created"
)

// Envelope wraps every event published to the broker.
type Envelope struct {
	ID        string          `json:"event_id"`
	Type      string          `json:"event_type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type TrackPlayedData struct {
	UserID   string `json:"user_id"`
	TrackID  string `json:"track_id"`
	ArtistID string `json:"artist_id"`
	Title    string `json:"title"`
}

type TrackMilestoneData struct {
	TrackID   string `json:"track_id"`
	Title     string `json:"title"`
	PlayCount int    `json:"play_count"`
}

type PlaylistCreatedData struct {
	PlaylistID string `json:"playlist_id"`
	UserID     string `json:"user_id"`
	Name       string `json:"name"`
}

// NewEnvelope builds an event with a unique ID (used for duplicate detection).
func NewEnvelope(eventType string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		ID:        uuid.NewString(),
		Type:      eventType,
		Timestamp: time.Now().UTC(),
		Data:      raw,
	}, nil
}