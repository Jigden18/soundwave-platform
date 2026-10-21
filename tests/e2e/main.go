// SoundWave - Part G integration / system test runner.
//
// Runs every scenario required by the WEB303 Lab 6 test table against the
// RUNNING system (docker compose + the four Go services) and writes a
// Markdown report with expected vs ACTUAL results.
//
// Uses only the Go standard library. It does not touch any service code.
//
// Usage (from the repo root, with the whole system already running):
//
//	go run ./tests/e2e -tester "Your Name"
//	go run ./tests/e2e -only TC01,TC04          # run a subset
//	go run ./tests/e2e -skip-interactive        # skip scenarios that need you to stop/start a service
//	go run ./tests/e2e -dlq                     # also run the poison-message / dead-letter test
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- config

var (
	playbackURL     string
	analyticsURL    string
	playlistURL     string
	notificationURL string
	mgmtURL         string
	mgmtUser        string
	mgmtPass        string
	dockerBin       string
	rabbitContainer string
	milestone       int
	runID           string

	httpClient = &http.Client{Timeout: 10 * time.Second}
	stdin      = bufio.NewReader(os.Stdin)
	findings   []string
)

// state shared between the "consumer unavailable" and "consumer recovery" tests
var st struct {
	notifStopped bool
	offlineName  string
	offlineUser  string
}

// ---------------------------------------------------------------- test plumbing

type T struct {
	checks []string
	passes int
	fails  int
	skip   string
	result string
}

func (t *T) ok(cond bool, format string, a ...any) bool {
	msg := fmt.Sprintf(format, a...)
	if cond {
		t.passes++
		t.checks = append(t.checks, "[ok] "+msg)
		fmt.Println("      [ok]   " + msg)
	} else {
		t.fails++
		t.checks = append(t.checks, "[FAIL] "+msg)
		fmt.Println("      [FAIL] " + msg)
	}
	return cond
}

func (t *T) note(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	t.checks = append(t.checks, "[info] "+msg)
	fmt.Println("      [info] " + msg)
}

func (t *T) finding(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	t.checks = append(t.checks, "[FINDING] "+msg)
	findings = append(findings, msg)
	fmt.Println("      [FIND] " + msg)
}

func (t *T) skipf(format string, a ...any) { t.skip = fmt.Sprintf(format, a...) }

type Case struct {
	ID       string
	Scenario string
	Steps    string
	Expected string
	Shot     string // screenshot to attach manually
	Run      func(t *T)
}

type Result struct {
	Case   Case
	Status string
	Actual string
	Checks []string
	Took   time.Duration
}

// ---------------------------------------------------------------- HTTP helpers

func do(method, u string, body any) (int, []byte, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

func mgmtDo(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, mgmtURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(mgmtUser, mgmtPass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

func isUp(base string) bool {
	code, _, err := do("GET", base+"/health", nil)
	return err == nil && code == 200
}

func waitFor(timeout time.Duration, f func() (bool, string)) (bool, string, time.Duration) {
	start := time.Now()
	for {
		ok, info := f()
		if ok {
			return true, info, time.Since(start).Round(100 * time.Millisecond)
		}
		if time.Since(start) > timeout {
			return false, info, time.Since(start).Round(100 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func pause(lines ...string) error {
	fmt.Println()
	fmt.Println("  >>> ACTION REQUIRED")
	for _, l := range lines {
		fmt.Println("      " + l)
	}
	fmt.Print("      Press Enter when done... ")
	if _, err := stdin.ReadString('\n'); err != nil {
		return fmt.Errorf("no keyboard input available")
	}
	return nil
}

// ---------------------------------------------------------------- domain helpers

type play struct {
	ID      string `json:"id"`
	UserID  string `json:"user_id"`
	TrackID string `json:"track_id"`
}
type trackStat struct {
	TrackID   string `json:"track_id"`
	Title     string `json:"title"`
	ArtistID  string `json:"artist_id"`
	PlayCount int    `json:"play_count"`
}
type recent struct {
	TrackID string `json:"track_id"`
	Title   string `json:"title"`
}
type notif struct {
	ID      string `json:"id"`
	UserID  string `json:"user_id"`
	Type    string `json:"type"`
	Message string `json:"message"`
}
type playlist struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Tracks []struct {
		TrackID string `json:"track_id"`
	} `json:"tracks"`
}

func user(tc string) string  { return fmt.Sprintf("e2e-%s-%s", runID, tc) }
func track(tc string) string { return fmt.Sprintf("e2e-%s-%s", runID, tc) }

func postPlay(u, trk, artist, title string) (int, []byte, error) {
	return do("POST", playbackURL+"/plays", map[string]string{
		"user_id": u, "track_id": trk, "artist_id": artist, "title": title})
}

func postPlaylist(u, name string) (int, playlist, error) {
	code, body, err := do("POST", playlistURL+"/playlists", map[string]string{"user_id": u, "name": name})
	var p playlist
	if err == nil {
		_ = json.Unmarshal(body, &p)
	}
	return code, p, err
}

func getStat(trk string) *trackStat {
	code, body, err := do("GET", analyticsURL+"/stats/tracks/"+url.PathEscape(trk), nil)
	if err != nil || code != 200 {
		return nil
	}
	var s trackStat
	if json.Unmarshal(body, &s) != nil {
		return nil
	}
	return &s
}

func getRecent(u string) []recent {
	_, body, _ := do("GET", playlistURL+"/users/"+url.PathEscape(u)+"/recent", nil)
	var r []recent
	_ = json.Unmarshal(body, &r)
	return r
}

func getNotifs(u string) []notif {
	_, body, _ := do("GET", notificationURL+"/notifications?user_id="+url.QueryEscape(u), nil)
	var n []notif
	_ = json.Unmarshal(body, &n)
	return n
}

func findNotifs(u, substr string) []notif {
	var out []notif
	for _, n := range getNotifs(u) {
		if strings.Contains(n.Message, substr) {
			out = append(out, n)
		}
	}
	return out
}

func waitCount(trk string, want int, timeout time.Duration) (bool, string, time.Duration) {
	return waitFor(timeout, func() (bool, string) {
		s := getStat(trk)
		if s == nil {
			return false, "no stats yet"
		}
		return s.PlayCount == want, fmt.Sprintf("play_count=%d", s.PlayCount)
	})
}

// queueReady returns messages_ready for a queue. It uses `docker exec ... rabbitmqctl`
// (real time) and falls back to the management API (which can lag ~5 s).
func queueReady(name string) (int, error) {
	out, err := exec.Command(dockerBin, "exec", rabbitContainer, "rabbitmqctl", "list_queues", "name", "messages_ready").CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == name {
				if n, e := strconv.Atoi(f[1]); e == nil {
					return n, nil
				}
			}
		}
		err = fmt.Errorf("queue %q not found in rabbitmqctl output", name)
	}
	code, body, e2 := mgmtDo("GET", "/api/queues/%2F/"+name, nil)
	if e2 != nil || code != 200 {
		return 0, fmt.Errorf("docker exec failed (%v) and management API failed (%v, HTTP %d)", err, e2, code)
	}
	var q struct {
		MessagesReady int `json:"messages_ready"`
	}
	_ = json.Unmarshal(body, &q)
	return q.MessagesReady, nil
}

// publishEvent publishes a hand-made event to the exchange via the RabbitMQ
// management API (same as the "Publish message" box in the dashboard).
func publishEvent(routingKey string, env map[string]any) (bool, error) {
	payload, _ := json.Marshal(env)
	code, body, err := mgmtDo("POST", "/api/exchanges/%2F/soundwave.events/publish", map[string]any{
		"properties":       map[string]any{"delivery_mode": 2, "content_type": "application/json"},
		"routing_key":      routingKey,
		"payload":          string(payload),
		"payload_encoding": "string",
	})
	if err != nil {
		return false, err
	}
	if code != 200 {
		return false, fmt.Errorf("management API returned HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	var r struct {
		Routed bool `json:"routed"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Routed, nil
}

func fakeEvent(id, typ string, data map[string]any) map[string]any {
	return map[string]any{
		"event_id":   id,
		"event_type": typ,
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
		"data":       data,
	}
}

// ---------------------------------------------------------------- test cases

func tc01(t *T) {
	u, trk := user("tc01"), track("tc01")
	code, body, err := postPlay(u, trk, "a1", "E2E Track "+trk)
	if !t.ok(err == nil, "Playback service reachable on :8081 (%v)", err) {
		return
	}
	t.ok(code == 201, "POST /plays returned HTTP %d (expected 201; a 502 would mean the event was NOT published)", code)
	var p play
	_ = json.Unmarshal(body, &p)
	t.ok(p.ID != "" && p.UserID == u && p.TrackID == trk, "response contains a play id (%s) and echoes user_id/track_id", p.ID)
	_, body, _ = do("GET", playbackURL+"/plays?user_id="+url.QueryEscape(u), nil)
	var plays []play
	_ = json.Unmarshal(body, &plays)
	found := false
	for _, x := range plays {
		if x.ID == p.ID {
			found = true
		}
	}
	t.ok(found, "the play is stored in Playback's own database (GET /plays?user_id=%s lists it)", u)
	t.note("Manual evidence: Playback terminal shows '[publish] type=track.played id=...'")
	t.result = fmt.Sprintf("HTTP %d, play id %s saved; API returned 201 so the broker confirmed the publish.", code, p.ID)
}

func tc02(t *T) {
	u, trk := user("tc02"), track("tc02")
	code, _, err := postPlay(u, trk, "a1", "E2E Track "+trk)
	if !t.ok(err == nil && code == 201, "POST /plays accepted (HTTP %d, err %v)", code, err) {
		return
	}
	ok, info, d := waitCount(trk, 1, 10*time.Second)
	t.ok(ok, "Analytics consumed track.played and stored the count (%s) after %s", info, d)
	s := getStat(trk)
	if s != nil {
		t.ok(s.Title == "E2E Track "+trk && s.ArtistID == "a1", "event carried enough data (title=%q artist_id=%q) - no call back to Playback needed", s.Title, s.ArtistID)
	}
	t.note("Manual evidence: Analytics terminal shows 'track=%s plays=1'", trk)
	t.result = fmt.Sprintf("Analytics updated asynchronously: %s, visible %s after the request.", info, d)
}

func tc03(t *T) {
	u, trk := user("tc03"), track("tc03")
	title := "E2E Track " + trk
	code, _, err := postPlay(u, trk, "a1", title)
	if !t.ok(err == nil && code == 201, "one POST /plays accepted (HTTP %d, err %v)", code, err) {
		return
	}
	okA, infoA, dA := waitCount(trk, 1, 10*time.Second)
	t.ok(okA, "consumer 1 - Analytics: %s after %s", infoA, dA)
	okP, infoP, dP := waitFor(10*time.Second, func() (bool, string) {
		n := 0
		for _, r := range getRecent(u) {
			if r.TrackID == trk {
				n++
			}
		}
		return n == 1, fmt.Sprintf("%d matching recent entries", n)
	})
	t.ok(okP, "consumer 2 - Playlist: /users/%s/recent lists the track (%s) after %s", u, infoP, dP)
	t.note("Manual evidence: Analytics and Playlist terminals both log the SAME event id for this play")
	t.result = fmt.Sprintf("One event, two services updated: Analytics (%s) and Playlist (%s).", infoA, infoP)
}

func tc04(t *T) {
	u, trk := user("tc04"), track("tc04")
	title := "E2E Milestone " + runID
	doPlay := func() bool {
		code, _, err := postPlay(u, trk, "a1", title)
		return err == nil && code == 201
	}
	t.note("milestone is every %d plays; testing %d, %d and %d plays (boundary values)", milestone, milestone-1, milestone, milestone+1)
	for i := 1; i < milestone; i++ {
		if !t.ok(doPlay(), "play #%d accepted (HTTP 201)", i) {
			return
		}
	}
	ok, info, _ := waitCount(trk, milestone-1, 10*time.Second)
	t.ok(ok, "Analytics reached %d plays (%s)", milestone-1, info)
	time.Sleep(2 * time.Second)
	n := len(findNotifs(u, title))
	t.ok(n == 0, "at %d plays: NO milestone notification yet (found %d)", milestone-1, n)

	t.ok(doPlay(), "play #%d accepted (HTTP 201)", milestone)
	var got []notif
	ok, info, d := waitFor(15*time.Second, func() (bool, string) {
		got = findNotifs(u, title)
		return len(got) >= 1, fmt.Sprintf("%d notification(s)", len(got))
	})
	t.ok(ok, "at %d plays: milestone notification appeared (%s) after %s", milestone, info, d)
	if len(got) > 0 {
		nt := got[0]
		t.ok(nt.Type == "track.milestone_reached", "notification type is %q", nt.Type)
		t.ok(nt.UserID == "all", "notification is a broadcast (user_id=%q)", nt.UserID)
		t.ok(strings.Contains(nt.Message, fmt.Sprintf("reached %d plays", milestone)), "message: %s", nt.Message)
		t.note("notification id (= event id of track.milestone_reached): %s", nt.ID)
	}

	t.ok(doPlay(), "play #%d accepted (HTTP 201)", milestone+1)
	waitCount(trk, milestone+1, 10*time.Second)
	time.Sleep(2 * time.Second)
	n = len(findNotifs(u, title))
	t.ok(n == 1, "at %d plays: still exactly 1 milestone notification (found %d)", milestone+1, n)
	t.note("Manual evidence: terminals show Analytics 'milestone reached', then Notification 'created for user=all'")
	t.result = fmt.Sprintf("track.played -> Analytics -> track.milestone_reached -> Notification. 0 notifications at %d plays, 1 at %d, still 1 at %d.", milestone-1, milestone, milestone+1)
}

func tc05(t *T) {
	base, err := queueReady("notification.queue")
	if !t.ok(err == nil, "can read notification.queue depth (%v)", err) {
		return
	}
	t.note("baseline notification.queue messages_ready = %d", base)
	if err := pause(
		"Stop the NOTIFICATION service: go to its terminal and press Ctrl+C.",
		"Leave Playback, Analytics and Playlist running."); err != nil {
		t.skipf("%v (run without -skip-interactive in a real terminal)", err)
		return
	}
	st.notifStopped = true
	down, _, _ := waitFor(15*time.Second, func() (bool, string) { return !isUp(notificationURL), "" })
	if !t.ok(down, "Notification service is down (health check on :8084 fails)") {
		st.notifStopped = false
		return
	}

	u, name := user("tc05"), "E2E Offline "+runID
	code, p, err := postPlaylist(u, name)
	t.ok(err == nil && code == 201, "POST /playlists still succeeds while Notification is down (HTTP %d)", code)
	st.offlineName, st.offlineUser = name, u

	var last int
	ok, _, d := waitFor(15*time.Second, func() (bool, string) {
		n, e := queueReady("notification.queue")
		last = n
		return e == nil && n >= base+1, ""
	})
	t.ok(ok, "playlist.created waits in notification.queue (messages_ready %d -> %d after %s)", base, last, d)
	t.ok(isUp(playbackURL) && isUp(analyticsURL) && isUp(playlistURL), "Playback, Analytics and Playlist keep running")
	_, body, _ := do("GET", playlistURL+"/playlists/"+p.ID, nil)
	var got playlist
	_ = json.Unmarshal(body, &got)
	t.ok(got.ID == p.ID, "Playlist service still serves the new playlist")

	trk := track("tc05")
	code, _, _ = postPlay(u, trk, "a1", "E2E Track "+trk)
	okA, infoA, _ := waitCount(trk, 1, 10*time.Second)
	t.ok(code == 201 && okA, "unrelated flow track.played -> Analytics still works during the outage (%s)", infoA)
	t.note("Manual evidence: screenshot the 'rabbitmqctl list_queues name messages_ready' output and the dashboard Queues tab")
	t.result = fmt.Sprintf("API stayed available (201); the event was held in notification.queue (%d -> %d); other services unaffected.", base, last)
}

func tc06(t *T) {
	if !st.notifStopped {
		t.skipf("TC05 did not stop the Notification service, so there is nothing to recover")
		return
	}
	if err := pause(
		"Start the NOTIFICATION service again (go run ./services/notification).",
		"Wait until its terminal prints '[notification] listening on :8084'."); err != nil {
		t.skipf("%v", err)
		return
	}
	up, _, d := waitFor(120*time.Second, func() (bool, string) { return isUp(notificationURL), "" })
	if !t.ok(up, "Notification healthy again after %s", d) {
		return
	}
	st.notifStopped = false
	var last int
	ok, _, d := waitFor(30*time.Second, func() (bool, string) {
		n, e := queueReady("notification.queue")
		last = n
		return e == nil && n == 0, ""
	})
	t.ok(ok, "backlog drained: notification.queue messages_ready = %d within %s", last, d)
	var got []notif
	ok, _, d2 := waitFor(15*time.Second, func() (bool, string) {
		got = findNotifs(st.offlineUser, st.offlineName)
		return len(got) == 1, ""
	})
	t.ok(ok, "the notification missed during the outage now exists exactly once (found %d) after %s", len(got), d2)
	t.note("Manual evidence: Notification terminal shows 'created for user=%s ...'", st.offlineUser)
	t.result = fmt.Sprintf("After restart the queue drained to %d and the missed notification was created (1 copy).", last)
}

func tc07(t *T) {
	u, name := user("tc07"), "E2E Duplicate "+runID
	id := "e2e-dup-" + runID
	ev := fakeEvent(id, "playlist.created", map[string]any{"playlist_id": "p-" + id, "user_id": u, "name": name})
	for i := 1; i <= 2; i++ {
		routed, err := publishEvent("playlist.created", ev)
		if !t.ok(err == nil && routed, "publish #%d of event_id=%s routed to a queue (err %v)", i, id, err) {
			t.note("If routed=false: Notification must be running so notification.queue exists")
			return
		}
	}
	ok, _, d := waitFor(15*time.Second, func() (bool, string) { return len(findNotifs(u, name)) >= 1, "" })
	t.ok(ok, "notification created after %s", d)
	time.Sleep(3 * time.Second)
	got := findNotifs(u, name)
	t.ok(len(got) == 1, "exactly ONE notification despite two deliveries (found %d)", len(got))
	if len(got) == 1 {
		t.ok(got[0].ID == id, "the stored notification id equals the event id (%s)", got[0].ID)
	}
	t.note("Manual evidence: Notification terminal shows 'duplicate event ignored id=%s'", id)
	t.result = fmt.Sprintf("Same event delivered twice -> %d notification stored; second delivery ignored.", len(got))
}

func tc08(t *T) {
	u, trk := user("tc08"), track("tc08")
	id := "e2e-dup-play-" + runID
	ev := fakeEvent(id, "track.played", map[string]any{"user_id": u, "track_id": trk, "artist_id": "a1", "title": "E2E Track " + trk})
	for i := 1; i <= 2; i++ {
		routed, err := publishEvent("track.played", ev)
		if !t.ok(err == nil && routed, "publish #%d of event_id=%s routed (err %v)", i, id, err) {
			return
		}
	}
	ok, _, d := waitCount(trk, 1, 15*time.Second)
	t.ok(ok, "Analytics processed the event after %s", d)
	time.Sleep(3 * time.Second)
	s := getStat(trk)
	cnt := -1
	if s != nil {
		cnt = s.PlayCount
	}
	t.ok(cnt == 1, "Analytics play_count is 1, not 2 (processed_events table blocked the duplicate): %d", cnt)
	n := 0
	for _, r := range getRecent(u) {
		if r.TrackID == trk {
			n++
		}
	}
	t.ok(n == 1, "Playlist recently-played has 1 entry, not 2 (event_id primary key): %d", n)
	t.note("Manual evidence: Analytics and Playlist terminals both print 'duplicate event ignored'")
	t.result = fmt.Sprintf("Duplicate track.played: Analytics count=%d, Playlist entries=%d (both correct = 1).", cnt, n)
}

func tc09(t *T) {
	u, trk := user("tc09"), track("tc09")
	title := "E2E Workflow Track " + runID
	name := "E2E Workflow " + runID

	code, p, err := postPlaylist(u, name)
	if !t.ok(err == nil && code == 201 && p.ID != "", "step 1: create playlist (HTTP %d)", code) {
		return
	}
	for _, tk := range []string{"w1", "w2", "w1"} { // w1 twice: adding the same track again must not duplicate it
		c, _, _ := do("POST", playlistURL+"/playlists/"+p.ID+"/tracks", map[string]string{"track_id": tk, "title": "Song " + tk, "artist_id": "a1"})
		t.ok(c == 201, "step 2: add track %s to playlist (HTTP %d)", tk, c)
	}
	_, body, _ := do("GET", playlistURL+"/playlists/"+p.ID, nil)
	var got playlist
	_ = json.Unmarshal(body, &got)
	t.ok(len(got.Tracks) == 2, "step 3: playlist holds 2 distinct tracks (adding w1 twice did not duplicate) - found %d", len(got.Tracks))

	for i := 1; i <= milestone; i++ {
		c, _, _ := postPlay(u, trk, "a1", title)
		t.ok(c == 201, "step 4: play #%d (HTTP %d)", i, c)
	}
	ok, info, _ := waitCount(trk, milestone, 15*time.Second)
	t.ok(ok, "step 5: Analytics count rose to %d (%s)", milestone, info)
	n := 0
	waitFor(10*time.Second, func() (bool, string) {
		n = 0
		for _, r := range getRecent(u) {
			if r.TrackID == trk {
				n++
			}
		}
		return n == milestone, ""
	})
	t.ok(n == milestone, "step 6: Playlist 'recent' lists %d plays (found %d)", milestone, n)
	ok, _, d := waitFor(15*time.Second, func() (bool, string) {
		return len(findNotifs(u, title)) >= 1 && len(findNotifs(u, name)) >= 1, ""
	})
	t.ok(ok, "step 7: Notification shows both 'playlist created' and 'reached %d plays' after %s", milestone, d)
	code, body, _ = do("GET", analyticsURL+"/stats/top", nil)
	var top []trackStat
	_ = json.Unmarshal(body, &top)
	t.ok(code == 200 && len(top) > 0, "step 8: GET /stats/top returns a ranking (%d rows)", len(top))
	t.note("Manual evidence: repeat this workflow in the UI (Part F) and screenshot each step")
	t.result = "Playlist created and filled, 5 plays counted by Analytics, recent plays built by Playlist, both notifications delivered."
}

func tc10(t *T) {
	u := user("tc10")
	t.note("Equivalence classes: valid input, each missing required field, empty string, malformed JSON")
	type c struct {
		name, method, url string
		body              any
		want              int
	}
	valid := map[string]string{"user_id": u, "track_id": track("tc10"), "title": "T"}
	without := func(k string) map[string]string {
		m := map[string]string{}
		for a, b := range valid {
			if a != k {
				m[a] = b
			}
		}
		return m
	}
	empty := func(k string) map[string]string {
		m := without(k)
		m[k] = ""
		return m
	}
	cases := []c{
		{"Playback: malformed JSON", "POST", playbackURL + "/plays", "{bad json", 400},
		{"Playback: empty object", "POST", playbackURL + "/plays", map[string]string{}, 400},
		{"Playback: missing user_id", "POST", playbackURL + "/plays", without("user_id"), 400},
		{"Playback: missing track_id", "POST", playbackURL + "/plays", without("track_id"), 400},
		{"Playback: missing title", "POST", playbackURL + "/plays", without("title"), 400},
		{"Playback: empty title", "POST", playbackURL + "/plays", empty("title"), 400},
		{"Playlist: malformed JSON", "POST", playlistURL + "/playlists", "{bad json", 400},
		{"Playlist: missing name", "POST", playlistURL + "/playlists", map[string]string{"user_id": u}, 400},
		{"Playlist: missing user_id", "POST", playlistURL + "/playlists", map[string]string{"name": "x"}, 400},
		{"Playlist: empty name", "POST", playlistURL + "/playlists", map[string]string{"user_id": u, "name": ""}, 400},
	}
	for _, x := range cases {
		code, body, err := do(x.method, x.url, x.body)
		msg := ""
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil {
			msg = e.Error
		}
		t.ok(err == nil && code == x.want && msg != "", "%s -> HTTP %d with error text %q (expected %d)", x.name, code, msg, x.want)
	}
	// valid input with the optional field omitted (valid equivalence class)
	code, _, _ := postPlay(u, track("tc10-opt"), "", "No artist")
	t.ok(code == 201, "Playback: optional artist_id omitted -> HTTP %d (expected 201)", code)

	// missing field when adding a track to a REAL playlist
	_, p, _ := postPlaylist(u, "E2E Validation "+runID)
	code, _, _ = do("POST", playlistURL+"/playlists/"+p.ID+"/tracks", map[string]string{"track_id": "x"})
	t.ok(code == 400, "Playlist: add track without title -> HTTP %d (expected 400)", code)

	// extra equivalence class: whitespace-only text (reported, not asserted)
	code, _, _ = postPlay(u, track("tc10-ws"), "a1", "   ")
	if code == 201 {
		t.finding("Playback accepts a whitespace-only title (HTTP 201). Validation only rejects empty strings. Suggest strings.TrimSpace before the check.")
	} else {
		t.note("whitespace-only title is rejected (HTTP %d)", code)
	}
	code, _, _ = postPlaylist(u, "   ")
	if code == 201 {
		t.finding("Playlist accepts a whitespace-only name (HTTP 201). Same cause as above.")
	} else {
		t.note("whitespace-only playlist name is rejected (HTTP %d)", code)
	}
	t.result = fmt.Sprintf("%d invalid-input classes rejected with HTTP 400 and an error message; valid class accepted.", len(cases)+1)
}

func tc11(t *T) {
	missing := "does-not-exist-" + runID
	type c struct {
		name, method, url string
		body              any
		want              int
	}
	cases := []c{
		{"Analytics: unknown track", "GET", analyticsURL + "/stats/tracks/" + missing, nil, 404},
		{"Playlist: unknown playlist", "GET", playlistURL + "/playlists/" + missing, nil, 404},
		{"Playlist: add track to unknown playlist", "POST", playlistURL + "/playlists/" + missing + "/tracks", map[string]string{"track_id": "x", "title": "x"}, 404},
		{"Playlist: recent plays of unknown user", "GET", playlistURL + "/users/" + missing + "/recent", nil, 200},
		{"Playback: plays of unknown user", "GET", playbackURL + "/plays?user_id=" + missing, nil, 200},
		{"Notification: unknown user", "GET", notificationURL + "/notifications?user_id=" + missing, nil, 200},
	}
	for _, x := range cases {
		code, body, err := do(x.method, x.url, x.body)
		t.ok(err == nil && code == x.want, "%s -> HTTP %d (expected %d)", x.name, code, x.want)
		if x.want == 200 {
			var arr []any
			t.ok(json.Unmarshal(body, &arr) == nil, "%s -> body is a JSON list (not null/garbage)", x.name)
		}
	}
	t.result = "Unknown ids give 404 with an error; unknown users give 200 with an empty/valid list."
}

func tc12(t *T) {
	base, err := queueReady("notification.queue.dlq")
	if !t.ok(err == nil, "can read notification.queue.dlq depth (%v)", err) {
		return
	}
	t.note("baseline notification.queue.dlq messages_ready = %d", base)
	if err := pause(
		"Stop the NOTIFICATION service (Ctrl+C), then restart it WITH the failure switch:",
		"  bash/WSL:    SIMULATE_FAILURE_FOR=Poison go run ./services/notification",
		"  PowerShell:  $env:SIMULATE_FAILURE_FOR=\"Poison\"; go run ./services/notification"); err != nil {
		t.skipf("%v", err)
		return
	}
	st.notifStopped = true
	up, _, _ := waitFor(120*time.Second, func() (bool, string) { return isUp(notificationURL), "" })
	if !t.ok(up, "Notification is up again with SIMULATE_FAILURE_FOR=Poison") {
		return
	}
	u, name := user("tc12"), "Poison Playlist "+runID
	code, _, _ := postPlaylist(u, name)
	t.ok(code == 201, "playlist '%s' created (HTTP %d)", name, code)
	t.note("waiting for 3 retries (2 s apart) and dead-lettering...")
	var last int
	ok, _, d := waitFor(60*time.Second, func() (bool, string) {
		n, e := queueReady("notification.queue.dlq")
		last = n
		return e == nil && n >= base+1, ""
	})
	t.ok(ok, "message moved to notification.queue.dlq (%d -> %d) after %s", base, last, d)
	t.ok(len(findNotifs(u, name)) == 0, "no notification was created for the poison message")
	live, _ := queueReady("notification.queue")
	t.ok(live == 0, "main notification.queue is not blocked (messages_ready=%d)", live)
	t.note("Manual evidence: Notification terminal shows 'error (retry 1/3)', '(retry 2/3)', '(retry 3/3)', then 'failed 3 times -> DLQ'")
	_ = pause(
		"Now stop the NOTIFICATION service again (Ctrl+C) and restart it WITHOUT the variable:",
		"  go run ./services/notification")
	waitFor(120*time.Second, func() (bool, string) { return isUp(notificationURL), "" })
	t.result = fmt.Sprintf("Poison message retried 3 times then dead-lettered (DLQ %d -> %d); main queue not blocked.", base, last)
}

// ---------------------------------------------------------------- report

func esc(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", "<br>")
}

func writeReport(path, tester string, results []*Result) {
	var b strings.Builder
	pass, fail, skip := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		default:
			skip++
		}
	}
	fmt.Fprintf(&b, "# SoundWave - Integration and System Test Report (Part G)\n\n")
	fmt.Fprintf(&b, "- Run: %s (run id `%s`)\n- Tester: %s\n- Milestone setting: every %d plays\n", time.Now().Format("2006-01-02 15:04:05"), runID, tester, milestone)
	fmt.Fprintf(&b, "- Result: **%d passed, %d failed, %d skipped**\n\n", pass, fail, skip)
	fmt.Fprintf(&b, "## Test table\n\n| ID | Scenario | Steps | Expected result | Actual result | Status | Screenshot |\n|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | **%s** | %s |\n", r.Case.ID, esc(r.Case.Scenario), esc(r.Case.Steps),
			esc(r.Case.Expected), esc(r.Actual), r.Status, esc("TODO: "+r.Case.Shot))
	}
	fmt.Fprintf(&b, "\n## Detailed checks\n")
	for _, r := range results {
		fmt.Fprintf(&b, "\n### %s - %s (%s, %s)\n\n", r.Case.ID, r.Case.Scenario, r.Status, r.Took.Round(100*time.Millisecond))
		for _, c := range r.Checks {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(findings) > 0 {
		fmt.Fprintf(&b, "\n## Findings to report to the service owner\n\n")
		for _, f := range findings {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Println("could not write report:", err)
		return
	}
	fmt.Println("Report written to", path)
}

// ---------------------------------------------------------------- main

func main() {
	flag.StringVar(&playbackURL, "playback", "http://localhost:8081", "Playback base URL")
	flag.StringVar(&analyticsURL, "analytics", "http://localhost:8082", "Analytics base URL")
	flag.StringVar(&playlistURL, "playlist", "http://localhost:8083", "Playlist base URL")
	flag.StringVar(&notificationURL, "notification", "http://localhost:8084", "Notification base URL")
	flag.StringVar(&mgmtURL, "mgmt", "http://localhost:15672", "RabbitMQ management URL")
	flag.StringVar(&mgmtUser, "mgmt-user", "guest", "RabbitMQ management user")
	flag.StringVar(&mgmtPass, "mgmt-pass", "guest", "RabbitMQ management password")
	flag.StringVar(&dockerBin, "docker", "docker", "docker executable")
	flag.StringVar(&rabbitContainer, "container", "soundwave-rabbitmq", "RabbitMQ container name")
	flag.IntVar(&milestone, "milestone", 5, "MILESTONE_EVERY value Analytics is running with")
	tester := flag.String("tester", "(your name)", "name printed in the report")
	only := flag.String("only", "", "comma-separated test ids to run, e.g. TC01,TC04")
	skipInteractive := flag.Bool("skip-interactive", false, "skip tests that need you to stop/start a service (TC05, TC06)")
	withDLQ := flag.Bool("dlq", false, "also run the poison-message / dead-letter test (TC12)")
	outDir := flag.String("out", "tests/results", "folder for the report")
	flag.Parse()

	if milestone < 2 {
		fmt.Println("-milestone must be at least 2")
		os.Exit(2)
	}
	runID = time.Now().Format("0102-150405")

	cases := []Case{
		{"TC01", "Successful business operation",
			"1. POST /plays {user_id, track_id, artist_id, title} to Playback (:8081)\n2. GET /plays?user_id=... on Playback",
			"HTTP 201 with the saved play; play stored in Playback's database; the event was published (no 502).",
			"Playback terminal with the '[publish] type=track.played' line + curl/UI response", tc01},
		{"TC02", "Event delivery",
			"1. POST /plays\n2. Poll GET /stats/tracks/{id} on Analytics (:8082)",
			"Analytics receives track.played and play_count becomes 1 without being called directly.",
			"Analytics terminal 'track=... plays=1' + /stats response", tc02},
		{"TC03", "Multiple consumers",
			"1. POST /plays once\n2. GET /stats/tracks/{id} (Analytics)\n3. GET /users/{id}/recent (Playlist)",
			"Both Analytics and Playlist process the same track.played event.",
			"Analytics and Playlist terminals side by side showing the same event id", tc03},
		{"TC04", "Event chaining (with boundary values 4/5/6 plays)",
			"1. Play one track 4 times; check notifications\n2. 5th play; poll /notifications\n3. 6th play; check notifications again",
			"No milestone at 4 plays; exactly one 'reached 5 plays' notification at 5; no second one at 6.",
			"Analytics 'milestone reached' log, Notification 'created' log, /notifications response", tc04},
		{"TC05", "Consumer unavailable",
			"1. Stop Notification (Ctrl+C)\n2. POST /playlists\n3. rabbitmqctl list_queues name messages_ready",
			"POST succeeds (201); notification.queue holds the waiting message; other services keep working.",
			"rabbitmqctl output showing notification.queue with messages_ready >= 1", tc05},
		{"TC06", "Consumer recovery",
			"1. Start Notification again\n2. Watch notification.queue\n3. GET /notifications",
			"Queue drops to 0 and the missed notification appears exactly once.",
			"rabbitmqctl output showing 0 + Notification terminal processing the backlog", tc06},
		{"TC07", "Duplicate event (playlist.created -> Notification)",
			"1. Publish the same playlist.created event twice via the RabbitMQ management API\n2. GET /notifications",
			"One notification only; log says 'duplicate event ignored'.",
			"Notification terminal 'duplicate event ignored' + dashboard publish dialog", tc07},
		{"TC08", "Duplicate event (track.played -> Analytics and Playlist)",
			"1. Publish the same track.played event twice\n2. GET /stats/tracks/{id}; GET /users/{id}/recent",
			"Analytics count is 1 (not 2); Playlist has one recent entry (not 2).",
			"Analytics and Playlist 'duplicate event ignored' logs", tc08},
		{"TC09", "Complete user workflow (API level)",
			"1. Create playlist\n2. Add tracks (one twice)\n3. Play a track 5 times\n4. Read stats, recent and notifications",
			"All services reflect the workflow: playlist + tracks, count 5, 5 recent plays, playlist and milestone notifications.",
			"The same workflow performed in the UI with each panel updating", tc09},
		{"TC10", "Input validation (equivalence classes)",
			"POST invalid bodies to /plays and /playlists: malformed JSON, empty object, each missing field, empty strings",
			"HTTP 400 with {\"error\": ...}; valid input (optional field omitted) accepted.",
			"Terminal output of this test run", tc10},
		{"TC11", "Unknown ids and users",
			"GET/POST using ids that do not exist",
			"404 with an error for unknown track/playlist; 200 with a valid list for unknown users.",
			"Terminal output of this test run", tc11},
	}
	if *withDLQ {
		cases = append(cases, Case{"TC12", "Poison message -> retries -> dead-letter queue",
			"1. Restart Notification with SIMULATE_FAILURE_FOR=Poison\n2. Create playlist 'Poison Playlist ...'\n3. Watch logs and notification.queue.dlq",
			"3 retries then the message lands in notification.queue.dlq; main queue is not blocked.",
			"Notification log with retry 1/3..3/3 and 'failed 3 times -> DLQ' + dashboard showing the DLQ message", tc12})
	}

	wanted := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		if id = strings.TrimSpace(strings.ToUpper(id)); id != "" {
			wanted[id] = true
		}
	}

	fmt.Printf("SoundWave integration tests - run %s\n", runID)
	fmt.Println("Pre-flight checks")
	pre := true
	for name, base := range map[string]string{"playback": playbackURL, "analytics": analyticsURL, "playlist": playlistURL, "notification": notificationURL} {
		up := isUp(base)
		fmt.Printf("  %-13s %s -> %v\n", name, base, map[bool]string{true: "healthy", false: "NOT REACHABLE"}[up])
		pre = pre && up
	}
	for _, q := range []string{"analytics.queue", "playlist.queue", "notification.queue"} {
		n, err := queueReady(q)
		if err != nil {
			fmt.Printf("  queue %-20s NOT FOUND (%v)\n", q, err)
			pre = false
		} else {
			fmt.Printf("  queue %-20s exists, %d ready\n", q, n)
		}
	}
	if !pre {
		fmt.Println("\nPre-flight failed. Check that: docker compose is up, `docker` works in this terminal,")
		fmt.Println("all four services are running (start the consumers first), and RabbitMQ has created the queues.")
		os.Exit(2)
	}

	var results []*Result
	for _, c := range cases {
		if len(wanted) > 0 && !wanted[c.ID] {
			continue
		}
		fmt.Printf("\n=== %s: %s\n", c.ID, c.Scenario)
		t := &T{}
		start := time.Now()
		if *skipInteractive && (c.ID == "TC05" || c.ID == "TC06" || c.ID == "TC12") {
			t.skipf("skipped (-skip-interactive)")
		} else {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.ok(false, "test crashed: %v", r)
					}
				}()
				c.Run(t)
			}()
		}
		r := &Result{Case: c, Checks: t.checks, Took: time.Since(start)}
		switch {
		case t.skip != "":
			r.Status, r.Actual = "SKIPPED", t.skip
		case t.fails > 0:
			r.Status = "FAIL"
			r.Actual = "FAILED - see the [FAIL] lines in the detailed checks."
			for _, ch := range t.checks {
				if strings.HasPrefix(ch, "[FAIL]") {
					r.Actual = "FAILED: " + strings.TrimPrefix(ch, "[FAIL] ")
					break
				}
			}
		default:
			r.Status = "PASS"
			r.Actual = t.result
			if r.Actual == "" {
				r.Actual = fmt.Sprintf("%d checks passed.", t.passes)
			}
		}
		fmt.Printf("    => %s\n", r.Status)
		results = append(results, r)
	}

	fmt.Println("\n================ SUMMARY ================")
	failed := 0
	for _, r := range results {
		fmt.Printf("  %-5s %-8s %s\n", r.Case.ID, r.Status, r.Case.Scenario)
		if r.Status == "FAIL" {
			failed++
		}
	}
	if len(findings) > 0 {
		fmt.Println("\nFindings:")
		for _, f := range findings {
			fmt.Println("  - " + f)
		}
	}
	writeReport(filepath.Join(*outDir, "e2e-report-"+time.Now().Format("20060102-150405")+".md"), *tester, results)
	if failed > 0 {
		os.Exit(1)
	}
}
