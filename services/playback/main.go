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

type Play struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	TrackID  string `json:"track_id"`
	ArtistID string `json:"artist_id"`
	Title    string `json:"title"`
	PlayedAt string `json:"played_at"`
}

var (
	db  *sql.DB
	bus *broker.Broker
)

func main() {
	var err error

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://soundwave:soundwave@localhost:5432/playback_db?sslmode=disable"
	}
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		log.Printf("[playback] waiting for database (%d/10): %v", i, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS plays (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		track_id TEXT NOT NULL,
		artist_id TEXT,
		title TEXT NOT NULL,
		played_at TEXT NOT NULL
	)`)
	if err != nil {
		log.Fatal(err)
	}

	bus, err = broker.Connect(broker.URL())
	if err != nil {
		log.Fatal(err)
	}
	defer bus.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "playback"})
	})
	mux.HandleFunc("POST /plays", createPlay)
	mux.HandleFunc("GET /plays", listPlays)

	log.Println("[playback] listening on :8081")
	log.Fatal(http.ListenAndServe(":8081", withCORS(mux)))
}

func createPlay(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID   string `json:"user_id"`
		TrackID  string `json:"track_id"`
		ArtistID string `json:"artist_id"`
		Title    string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if in.UserID == "" || in.TrackID == "" || in.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id, track_id and title are required"})
		return
	}

	p := Play{
		ID: uuid.NewString(), UserID: in.UserID, TrackID: in.TrackID,
		ArtistID: in.ArtistID, Title: in.Title,
		PlayedAt: time.Now().UTC().Format(time.RFC3339),
	}
	_, err := db.Exec(`INSERT INTO plays (id, user_id, track_id, artist_id, title, played_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, p.ID, p.UserID, p.TrackID, p.ArtistID, p.Title, p.PlayedAt)
	if err != nil {
		log.Printf("[playback] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save play"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err = bus.Publish(ctx, events.TrackPlayed, events.TrackPlayedData{
		UserID: p.UserID, TrackID: p.TrackID, ArtistID: p.ArtistID, Title: p.Title,
	})
	if err != nil {
		log.Printf("[playback] publish failed for play %s: %v", p.ID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "play saved but event could not be published"})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func listPlays(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	rows, err := db.Query(`SELECT id, user_id, track_id, artist_id, title, played_at
		FROM plays WHERE ($1::text = '' OR user_id = $1::text) ORDER BY played_at DESC LIMIT 50`, userID)
	if err != nil {
		log.Printf("[playback] query error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	plays := []Play{}
	for rows.Next() {
		var p Play
		if err := rows.Scan(&p.ID, &p.UserID, &p.TrackID, &p.ArtistID, &p.Title, &p.PlayedAt); err == nil {
			plays = append(plays, p)
		}
	}
	writeJSON(w, http.StatusOK, plays)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// CORS so your friends' UI can call this service from the browser.
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