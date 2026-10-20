package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"soundwave/pkg/broker"
	"soundwave/pkg/events"
)

type Track struct {
	TrackID  string `json:"track_id"`
	Title    string `json:"title"`
	ArtistID string `json:"artist_id"`
}

type Playlist struct {
	ID        string  `json:"id"`
	UserID    string  `json:"user_id"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"created_at"`
	Tracks    []Track `json:"tracks,omitempty"`
}

type RecentPlay struct {
	TrackID  string    `json:"track_id"`
	Title    string    `json:"title"`
	PlayedAt time.Time `json:"played_at"`
}

var (
	db  *sql.DB
	bus *broker.Broker
)

func main() {
	var err error
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://soundwave:soundwave@localhost:5432/playlist_db?sslmode=disable"
	}
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		log.Printf("[playlist] waiting for database (%d/10): %v", i, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		log.Fatal(err)
	}

	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS playlists (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS playlist_tracks (
			playlist_id TEXT NOT NULL,
			track_id TEXT NOT NULL,
			title TEXT NOT NULL,
			artist_id TEXT,
			PRIMARY KEY (playlist_id, track_id)
		)`,
		`CREATE TABLE IF NOT EXISTS recently_played (
			event_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			track_id TEXT NOT NULL,
			title TEXT NOT NULL,
			played_at TIMESTAMPTZ NOT NULL
		)`,
	} {
		if _, err = db.Exec(q); err != nil {
			log.Fatal(err)
		}
	}

	bus, err = broker.Connect(broker.URL())
	if err != nil {
		log.Fatal(err)
	}
	defer bus.Close()

	if err = bus.Consume("playlist.queue", []string{events.TrackPlayed}, handleTrackPlayed); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "playlist"})
	})
	mux.HandleFunc("POST /playlists", createPlaylist)
	mux.HandleFunc("GET /playlists", listPlaylists)
	mux.HandleFunc("GET /playlists/{id}", getPlaylist)
	mux.HandleFunc("POST /playlists/{id}/tracks", addTrack)
	mux.HandleFunc("GET /users/{id}/recent", recentPlays)

	log.Println("[playlist] listening on :8083")
	log.Fatal(http.ListenAndServe(":8083", withCORS(mux)))
}

// Event consumer: the event_id primary key makes duplicate deliveries harmless.
func handleTrackPlayed(env events.Envelope) error {
	var d events.TrackPlayedData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	res, err := db.Exec(`INSERT INTO recently_played (event_id, user_id, track_id, title, played_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (event_id) DO NOTHING`,
		env.ID, d.UserID, d.TrackID, d.Title, env.Timestamp)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		log.Printf("[playlist] duplicate event ignored id=%s", env.ID)
		return nil
	}
	log.Printf("[playlist] recorded recent play user=%s track=%s (event %s)", d.UserID, d.TrackID, env.ID)
	return nil
}

func createPlaylist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UserID == "" || in.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id and name are required"})
		return
	}
	p := Playlist{
		ID: uuid.NewString(), UserID: in.UserID, Name: in.Name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := db.Exec(`INSERT INTO playlists (id, user_id, name, created_at) VALUES ($1, $2, $3, $4)`,
		p.ID, p.UserID, p.Name, p.CreatedAt); err != nil {
		log.Printf("[playlist] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save playlist"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := bus.Publish(ctx, events.PlaylistCreated, events.PlaylistCreatedData{
		PlaylistID: p.ID, UserID: p.UserID, Name: p.Name,
	}); err != nil {
		log.Printf("[playlist] publish failed for playlist %s: %v", p.ID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "playlist saved but event could not be published"})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func listPlaylists(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	rows, err := db.Query(`SELECT id, user_id, name, created_at FROM playlists
		WHERE ($1::text = '' OR user_id = $1::text) ORDER BY created_at DESC`, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()
	list := []Playlist{}
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.CreatedAt); err == nil {
			list = append(list, p)
		}
	}
	writeJSON(w, http.StatusOK, list)
}

func getPlaylist(w http.ResponseWriter, r *http.Request) {
	var p Playlist
	err := db.QueryRow(`SELECT id, user_id, name, created_at FROM playlists WHERE id = $1`,
		r.PathValue("id")).Scan(&p.ID, &p.UserID, &p.Name, &p.CreatedAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "playlist not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	rows, err := db.Query(`SELECT track_id, title, COALESCE(artist_id, '') FROM playlist_tracks WHERE playlist_id = $1`, p.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t Track
			if rows.Scan(&t.TrackID, &t.Title, &t.ArtistID) == nil {
				p.Tracks = append(p.Tracks, t)
			}
		}
	}
	writeJSON(w, http.StatusOK, p)
}

func addTrack(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM playlists WHERE id = $1)`, id).Scan(&exists); err != nil || !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "playlist not found"})
		return
	}
	var t Track
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.TrackID == "" || t.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "track_id and title are required"})
		return
	}
	if _, err := db.Exec(`INSERT INTO playlist_tracks (playlist_id, track_id, title, artist_id)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, id, t.TrackID, t.Title, t.ArtistID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not add track"})
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func recentPlays(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT track_id, title, played_at FROM recently_played
		WHERE user_id = $1 ORDER BY played_at DESC LIMIT 10`, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()
	list := []RecentPlay{}
	for rows.Next() {
		var p RecentPlay
		if err := rows.Scan(&p.TrackID, &p.Title, &p.PlayedAt); err == nil {
			list = append(list, p)
		}
	}
	writeJSON(w, http.StatusOK, list)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}