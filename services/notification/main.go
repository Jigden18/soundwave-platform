package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"soundwave/pkg/broker"
	"soundwave/pkg/events"
)

type Notification struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	db  *sql.DB
	bus *broker.Broker
)

func main() {
	var err error
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://soundwave:soundwave@localhost:5432/notification_db?sslmode=disable"
	}
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		log.Printf("[notification] waiting for database (%d/10): %v", i, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS notifications (
		event_id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		type TEXT NOT NULL,
		message TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		log.Fatal(err)
	}

	bus, err = broker.Connect(broker.URL())
	if err != nil {
		log.Fatal(err)
	}
	defer bus.Close()

	if err = bus.Consume("notification.queue",
		[]string{events.TrackMilestoneReached, events.PlaylistCreated}, handleEvent); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "notification"})
	})
	mux.HandleFunc("GET /notifications", listNotifications)

	log.Println("[notification] listening on :8084")
	log.Fatal(http.ListenAndServe(":8084", withCORS(mux)))
}

func handleEvent(env events.Envelope) error {
	var userID, message string

	switch env.Type {
	case events.TrackMilestoneReached:
		var d events.TrackMilestoneData
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return err
		}
		userID = "all"
		message = fmt.Sprintf("\"%s\" has reached %d plays", d.Title, d.PlayCount)
	case events.PlaylistCreated:
		var d events.PlaylistCreatedData
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return err
		}
		userID = d.UserID
		message = fmt.Sprintf("Your playlist \"%s\" was created", d.Name)
	default:
		log.Printf("[notification] unknown event type %q ignored", env.Type)
		return nil
	}
	
	if f := os.Getenv("SIMULATE_FAILURE_FOR"); f != "" && strings.Contains(message, f) {
		return fmt.Errorf("simulated processing failure for %q", f)
	}

	res, err := db.Exec(`INSERT INTO notifications (event_id, user_id, type, message)
		VALUES ($1, $2, $3, $4) ON CONFLICT (event_id) DO NOTHING`, env.ID, userID, env.Type, message)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		log.Printf("[notification] duplicate event ignored id=%s", env.ID)
		return nil
	}
	log.Printf("[notification] created for user=%s type=%s: %s (event %s)", userID, env.Type, message, env.ID)
	return nil
}

func listNotifications(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	rows, err := db.Query(`SELECT event_id, user_id, type, message, created_at FROM notifications
		WHERE ($1::text = '' OR user_id = $1::text OR user_id = 'all')
		ORDER BY created_at DESC LIMIT 50`, userID)
	if err != nil {
		log.Printf("[notification] query error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()
	list := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Message, &n.CreatedAt); err == nil {
			list = append(list, n)
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