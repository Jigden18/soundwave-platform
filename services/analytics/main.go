package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"soundwave/pkg/broker"
	"soundwave/pkg/events"
)

type TrackStat struct {
	TrackID   string `json:"track_id"`
	Title     string `json:"title"`
	ArtistID  string `json:"artist_id"`
	PlayCount int    `json:"play_count"`
}

var (
	db             *sql.DB
	bus            *broker.Broker
	milestoneEvery = 5 // a milestone event is published at every 5th play
)

func main() {
	var err error
	if v, e := strconv.Atoi(os.Getenv("MILESTONE_EVERY")); e == nil && v > 0 {
		milestoneEvery = v
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://soundwave:soundwave@localhost:5432/analytics_db?sslmode=disable"
	}
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		log.Printf("[analytics] waiting for database (%d/10): %v", i, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		log.Fatal(err)
	}

	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS processed_events (
			event_id TEXT PRIMARY KEY,
			processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS track_stats (
			track_id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			artist_id TEXT,
			play_count INTEGER NOT NULL DEFAULT 0
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

	if err = bus.Consume("analytics.queue", []string{events.TrackPlayed}, handleTrackPlayed); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "analytics"})
	})
	mux.HandleFunc("GET /stats/top", topTracks)
	mux.HandleFunc("GET /stats/tracks/{id}", trackStat)

	log.Println("[analytics] listening on :8082")
	log.Fatal(http.ListenAndServe(":8082", withCORS(mux)))
}

func handleTrackPlayed(env events.Envelope) error {
	var d events.TrackPlayedData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after a successful Commit

	// Duplicate check: the event ID is recorded in the same transaction as the business change.
	res, err := tx.Exec(`INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`, env.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		log.Printf("[analytics] duplicate event ignored id=%s", env.ID)
		return nil
	}

	var count int
	err = tx.QueryRow(`INSERT INTO track_stats (track_id, title, artist_id, play_count)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (track_id) DO UPDATE SET play_count = track_stats.play_count + 1
		RETURNING play_count`, d.TrackID, d.Title, d.ArtistID).Scan(&count)
	if err != nil {
		return err
	}
	log.Printf("[analytics] track=%s plays=%d (event %s)", d.TrackID, count, env.ID)

	// Event chaining: publish before commit, so a failed publish rolls back and the event is retried.
	if count%milestoneEvery == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := bus.Publish(ctx, events.TrackMilestoneReached, events.TrackMilestoneData{
			TrackID: d.TrackID, Title: d.Title, PlayCount: count,
		}); err != nil {
			return err
		}
		log.Printf("[analytics] milestone reached track=%s plays=%d", d.TrackID, count)
	}
	return tx.Commit()
}

func topTracks(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT track_id, title, COALESCE(artist_id, ''), play_count
		FROM track_stats ORDER BY play_count DESC LIMIT 10`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()
	stats := []TrackStat{}
	for rows.Next() {
		var s TrackStat
		if err := rows.Scan(&s.TrackID, &s.Title, &s.ArtistID, &s.PlayCount); err == nil {
			stats = append(stats, s)
		}
	}
	writeJSON(w, http.StatusOK, stats)
}

func trackStat(w http.ResponseWriter, r *http.Request) {
	var s TrackStat
	err := db.QueryRow(`SELECT track_id, title, COALESCE(artist_id, ''), play_count
		FROM track_stats WHERE track_id = $1`, r.PathValue("id")).Scan(&s.TrackID, &s.Title, &s.ArtistID, &s.PlayCount)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "track not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	writeJSON(w, http.StatusOK, s)
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