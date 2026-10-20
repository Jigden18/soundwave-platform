# SoundWave: Team Guide (WEB303 Lab 6)

SoundWave is an event-driven, Spotify-like music platform built with Go microservices and RabbitMQ.
Parts A to E (design, services, events, failure handling) are **already implemented and verified**.
This guide tells everyone else how to run the system and complete **Parts F to J** without breaking it.

> The final report must describe the system we actually built. Do not change service behaviour, ports or event
> formats without telling the group first (see "Rules for working together").

---

## 1. Who does what

| Part | Responsibility | Owner |
|---|---|---|
| A, B | Application selection, service planning, event-driven design | Jigden Shakya |
| C, D, E | Microservices, event publishing and consuming, failure handling | Jigden Shakya |
| F | User interface | _name_ |
| G | Integration and system testing (test table with expected and actual results) | _name_ |
| H | Collaboration: shared understanding, integration, explaining contributions | Everyone |
| I | Presentation and live demo | Everyone |
| J | Report (single PDF) | Everyone, see section 9 |

Fill in the names above in the repo so the report can state individual contributions accurately.

---

## 2. Architecture in one minute

```
                         +-----------------------+
 UI  --HTTP-->  Playback |  POST /plays          |
                  :8081  |  saves play (Postgres)|
                         +-----------+-----------+
                                     | publishes  track.played
                                     v
                    RabbitMQ exchange "soundwave.events" (topic, durable)
                      |                         |
        analytics.queue                   playlist.queue
                      |                         |
              +-------v-------+         +-------v--------+
              |  Analytics    |         |   Playlist     |
              |  :8082        |         |   :8083        |
              | counts plays  |         | recently played|
              +-------+-------+         | + playlists    |
                      |                 +-------+--------+
   publishes track.milestone_reached            | publishes playlist.created
   (every 5th play of a track)                  |
                      |                         |
                      +-----------+-------------+
                                  v
                          notification.queue
                                  |
                          +-------v--------+
                          |  Notification  |
                          |  :8084         |
                          +----------------+
```

- **Multiple consumers:** `track.played` is processed by both Analytics and Playlist.
- **Event chaining:** `track.played` leads to Analytics publishing `track.milestone_reached`, which leads to a notification.
- Every service has its **own database** inside one Postgres container. Services never read each other's database.

---

## 3. Prerequisites

- Go 1.22 or newer (`go version`)
- Docker with Docker Compose (`docker compose version`)
- Git
- `curl` (for testing)

---

## 4. First-time setup

```bash
git clone <REPO_URL> soundwave
cd soundwave
go mod download
docker compose up -d
docker compose ps          # rabbitmq and postgres should both be "healthy"
go build ./...             # must print nothing
```

- RabbitMQ dashboard: http://localhost:15672 (login `guest` / `guest`)
- Postgres: `localhost:5432`, user `soundwave`, password `soundwave`
- Databases `playback_db`, `analytics_db`, `playlist_db`, `notification_db` are created automatically from `db/init.sql`
  the first time the Postgres volume is created.

### Starting the services

Use one terminal per service. **Start the consumers first**, so their queues exist before any event is published.
RabbitMQ drops an event if no queue is bound to it yet.

```bash
go run ./services/analytics      # terminal 1
go run ./services/playlist       # terminal 2
go run ./services/notification   # terminal 3
go run ./services/playback       # terminal 4
```

Quick health check:

```bash
for p in 8081 8082 8083 8084; do curl -s http://localhost:$p/health; echo; done
```

### Resetting everything (wipes all data and queues)

```bash
docker compose down -v
docker compose up -d
```

---

## 5. API reference

All APIs speak JSON. **CORS is enabled**, so a browser UI can call every service directly.
Errors look like `{"error": "message"}`.

### Playback (port 8081)

| Method | Path | Body or query | Result |
|---|---|---|---|
| POST | `/plays` | `{"user_id","track_id","artist_id","title"}` (`user_id`, `track_id`, `title` required) | `201` with the play. Publishes `track.played`. |
| GET | `/plays?user_id=u1` | optional `user_id` | last 50 plays |
| GET | `/health` | | status |

A `502` from `POST /plays` means: "the play was saved but the event could not be published". Show this to the user as a failure.

### Analytics (port 8082)

| Method | Path | Result |
|---|---|---|
| GET | `/stats/top` | top 10 tracks by `play_count` |
| GET | `/stats/tracks/{id}` | stats for one track, `404` if unknown |
| GET | `/health` | status |

### Playlist (port 8083)

| Method | Path | Body or query | Result |
|---|---|---|---|
| POST | `/playlists` | `{"user_id","name"}` | `201`. Publishes `playlist.created`. |
| GET | `/playlists?user_id=u1` | optional `user_id` | list of playlists |
| GET | `/playlists/{id}` | | playlist with its `tracks` |
| POST | `/playlists/{id}/tracks` | `{"track_id","title","artist_id"}` | `201`, `404` if the playlist does not exist |
| GET | `/users/{id}/recent` | | last 10 plays, built from `track.played` events |
| GET | `/health` | | status |

### Notification (port 8084)

| Method | Path | Result |
|---|---|---|
| GET | `/notifications?user_id=u1` | notifications for that user **plus** broadcast ones (`user_id: "all"`), newest first |
| GET | `/health` | status |

---

## 6. Events

Every event is wrapped in the same envelope. The shared Go definitions live in `pkg/events/events.go`.

```json
{
  "event_id": "2323020c-ec8c-474d-ac71-9b98d957a3f9",
  "event_type": "track.played",
  "timestamp": "2026-10-20T02:01:17Z",
  "data": { }
}
```

| Event | Published by | Consumed by | `data` fields | Purpose |
|---|---|---|---|---|
| `track.played` | Playback | Analytics, Playlist | `user_id`, `track_id`, `artist_id`, `title` | A user played a track |
| `track.milestone_reached` | Analytics | Notification | `track_id`, `title`, `play_count` | A track hit a multiple of 5 plays |
| `playlist.created` | Playlist | Notification | `playlist_id`, `user_id`, `name` | A user created a playlist |

Queues (each also has a `<name>.dlq` dead-letter queue):

| Queue | Bound to |
|---|---|
| `analytics.queue` | `track.played` |
| `playlist.queue` | `track.played` |
| `notification.queue` | `track.milestone_reached`, `playlist.created` |

### Reliability behaviour (useful for the report and the demo)

- Messages are **persistent** and queues are **durable**, so events wait for a consumer that is down.
- Consumers use **manual acknowledgements**. On an error the event is retried 3 times (2 seconds apart),
  then moved to the dead-letter queue.
- **Duplicate protection** uses the `event_id`: Analytics stores it in `processed_events`, Playlist and Notification
  use it as a primary key. A repeated event is logged and ignored.
- Important events and errors are logged by each service (`[publish]`, `[consume]`, `[analytics]`, ...).

### Configuration (environment variables)

| Variable | Used by | Default |
|---|---|---|
| `RABBITMQ_URL` | all services | `amqp://guest:guest@localhost:5672/` |
| `DATABASE_URL` | all services | local Postgres, the service's own database |
| `MILESTONE_EVERY` | Analytics | `5` |
| `SIMULATE_FAILURE_FOR` | Notification | unset. If set, a notification message containing that text fails on purpose (used for the DLQ demo) |

---

## 7. Instructions per remaining part

### Part F: User interface

Build any UI technology you like (plain HTML/JS, React, etc.). Put it in a new `ui/` folder.
Call the four services directly on ports 8081 to 8084.

The UI must:
1. Let a user perform the main operations: play a track, create a playlist, add a track to a playlist.
2. Display data returned by the services: top tracks (Analytics), recent plays and playlists (Playlist), notifications (Notification).
3. **Reflect event-driven changes.** Events are processed asynchronously, so a count or notification can appear a moment
   after the request returns. Refresh or poll about every 2 seconds, or refresh about 1 second after an action.
4. Show feedback for success and failure (for example a toast). Treat `4xx` and `5xx` responses as failures and show the `error` text.
5. Demonstrate one complete workflow across services, for example: play a track 5 times, watch the Analytics count rise,
   see the Recent list grow, then see the "reached 5 plays" notification appear.

Suggestions:
- There is no Catalog service, so hard-code a small list of tracks in the UI, for example:
  `{track_id: "t1", artist_id: "a1", title: "Blinding Lights"}`.
- Use a simple user picker with fixed IDs such as `u1` and `u2`.
- Milestone notifications are stored for `user_id: "all"` and are returned for every user.

### Part G: Integration and system testing

Create a table with **Scenario, Steps, Expected result, Actual result, Screenshot**. Cover every required scenario:

| Scenario | How to run it |
|---|---|
| Successful business operation | `POST /plays`; expect `201` and a `[publish] type=track.played` log line in Playback |
| Event delivery | Check the Analytics log shows `track=... plays=N` for that event |
| Multiple consumers | Analytics and Playlist logs show the **same event ID**; `/users/u1/recent` and `/stats/top` both change |
| Event chaining | Send plays until a multiple of 5 and see the Analytics milestone log, then the Notification log and `/notifications` |
| Consumer unavailable | Stop Notification (Ctrl+C), create a playlist, then run `docker exec soundwave-rabbitmq rabbitmqctl list_queues name messages_ready`; `notification.queue` shows the waiting message(s) |
| Consumer recovery | Start Notification again; the queue drops to 0 and the missed notification appears |
| Duplicate event | In the RabbitMQ dashboard: Exchanges, `soundwave.events`, Publish message. Routing key `playlist.created`, payload below, published **twice**. Expect one notification and a `duplicate event ignored` log line |
| Complete user workflow | The Part F workflow above, from the UI |

Duplicate event payload:

```json
{"event_id":"dup-test-001","event_type":"playlist.created","timestamp":"2026-10-20T02:30:00Z","data":{"playlist_id":"p-dup","user_id":"u1","name":"Duplicate Test"}}
```

Poison message and dead-letter queue (extra evidence): stop Notification, restart it with
`SIMULATE_FAILURE_FOR=Poison go run ./services/notification`, create a playlist named `Poison Playlist`,
and watch three retries followed by `failed 3 times -> DLQ`. Then `notification.queue.dlq` holds 1 message.
Restart Notification **without** the variable afterwards.

Record expected and actual results honestly. If something fails, tell Jigden so it can be fixed before the presentation.

### Part H: Collaboration
- Everyone must understand the whole flow in section 2, not just their own part.
- Be ready to explain your own contribution and to answer questions about any service.

### Part I: Presentation (suggested order)
1. Project overview (what SoundWave does)
2. Microservices architecture (the four services)
3. Event-driven communication (RabbitMQ, events, publishers and consumers)
4. Live demo of one full workflow through the UI, with RabbitMQ dashboard and service logs visible
5. Failure handling demo: stop Notification, generate events, show the backlog, restart, show recovery
6. Testing evidence (the Part G table)
7. Each member explains their own contribution

Before the demo, run the whole system from a clean start (`docker compose down -v`, `docker compose up -d`,
start consumers first) so you know it works from scratch.

---

## 8. Rules for working together

1. **Do not change `pkg/events/` or `pkg/broker/` without telling the group.** All four services depend on them and the report describes them.
2. Do not change ports, API paths or event names. The UI and tests rely on them.
3. Work on a branch, not on `main`:
   ```bash
   git checkout -b ui          # or: testing, report
   git add <your files>
   git commit -m "Add playlist page"
   git push -u origin ui
   ```
   Then open a Pull Request on GitHub and ask one other member to merge it.
4. Pull often: `git pull origin main`.
5. Never commit database files, `.env` files or screenshots with personal data. `.gitignore` already covers `*.db`.
6. Found a bug in a service? Open a GitHub Issue with the steps to reproduce and the log output, and tag Jigden.

---

## 9. Report (Part J): one PDF, shared effort

The report must contain:

- Project title (**SoundWave: Event-Driven Music Streaming Platform**) and group member details
- A short description of the application
- The microservices and their responsibilities (section 2 and the table below)
- An architecture diagram: services, message broker, publishers and consumers (the diagram in section 2 is a starting point)
- A description of each event type and its purpose (section 6)
- Why RabbitMQ was chosen
- Screenshots of the working application and event-driven communication
- Test cases, results and failure-handling evidence (Part G table plus the failure demo screenshots)
- Individual contributions of each member

| Service | Responsibility |
|---|---|
| Playback | Records plays; publishes `track.played` |
| Analytics | Counts plays per track; publishes `track.milestone_reached` |
| Playlist | Manages playlists; keeps recently played; publishes `playlist.created` |
| Notification | Creates notifications from milestone and playlist events |

Why RabbitMQ (short version for the report): it supports durable queues, persistent messages, manual acknowledgements,
publisher confirms and dead-letter queues out of the box, which are exactly the mechanisms Part E asks for.
Topic routing lets one event fan out to several services, each with its own queue.

Assign sections between members and record who wrote what in the "individual contributions" section.

---

## 10. Troubleshooting

| Problem | Fix |
|---|---|
| `docker pull` fails with `network is unreachable` and an IPv6 address | Disable IPv6: `sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1`, same for `.default.`, then `sudo service docker restart` (WSL) |
| `database "..._db" does not exist` | The init script only runs when the Postgres volume is first created. Run `docker compose down -v` then `docker compose up -d` |
| "Message published, but not routed" in the dashboard | Wrong exchange or routing key, or the consumer's queue does not exist yet. Start the consumer services first |
| `port already in use` | Another copy of that service is running. Stop it, then start again |
| Event published but nothing happens | Check the consumer is running and look at `docker exec soundwave-rabbitmq rabbitmqctl list_queues name messages_ready` |
| After restarting the RabbitMQ container, services stop consuming | Known limitation: services do not reconnect automatically. Restart the Go services |
| `docker compose` YAML error | Indentation problem. Use spaces only, with `rabbitmq:` and `postgres:` indented by exactly 2 spaces |

---

## 11. Definition of done (checklist before the presentation)

- [ ] Whole system starts from scratch following section 4
- [ ] UI covers play, playlist create, add track, top tracks, recent plays and notifications
- [ ] UI shows success and failure feedback and updates after events
- [ ] Part G table complete with expected and actual results and screenshots
- [ ] Failure demo rehearsed (stop Notification, backlog, restart, recovery)
- [ ] Report PDF complete and matches the implemented system
- [ ] Every member can explain the architecture and their own contribution
