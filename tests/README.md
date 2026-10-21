# Part G - Integration and System Testing (SoundWave)

Everything here only ADDS files. No service code, ports, API paths or event formats were changed.

| File | What it is |
|---|---|
| `tests/e2e/main.go` | Runs all Part G scenarios against the RUNNING system and writes a Markdown report (expected vs actual) |
| `pkg/events/events_test.go`, `pkg/broker/broker_test.go` | Unit tests for the shared packages (broker uses a spy acknowledger) |
| `services/*/main_test.go` | Unit tests for input validation, event-handler errors, CORS, milestone rule |

## 1. Run the integration tests (this is your Part G table)

Start the system exactly as in TEAM_GUIDE section 4 (consumers first). Then, in a **5th terminal at the repo root**:

```bash
go run ./tests/e2e -tester "Your Name" -dlq
```

Needs `docker` to work in that terminal (it uses `docker exec soundwave-rabbitmq rabbitmqctl ...`).

Options: `-only TC01,TC04` run some tests, `-skip-interactive` skip TC05/TC06/TC12, `-milestone N` if Analytics runs with `MILESTONE_EVERY=N`.

Two tests pause and ask YOU to act (this is the failure-and-recovery demo):

- **TC05**: press Ctrl+C in the Notification terminal, then Enter.
- **TC06**: restart Notification, wait for `listening on :8084`, then Enter.
- **TC12** (`-dlq`): restart Notification with `SIMULATE_FAILURE_FOR=Poison` (PowerShell: `$env:SIMULATE_FAILURE_FOR="Poison"; go run ./services/notification`), later restart it without.

Result: terminal summary + `tests/results/e2e-report-<time>.md`. The table already has Scenario, Steps, Expected, **Actual**, Status. Fill the Screenshot column yourself.

| ID | Required Part G scenario |
|---|---|
| TC01 | Successful business operation |
| TC02 | Event delivery |
| TC03 | Multiple consumers |
| TC04 | Event chaining (boundary values 4 / 5 / 6 plays) |
| TC05 | Consumer unavailable |
| TC06 | Consumer recovery |
| TC07, TC08 | Duplicate event (Notification; Analytics + Playlist) |
| TC09 | Complete user workflow (API level; repeat it in the UI for the screenshot) |
| TC10, TC11 | Extra: invalid input, unknown ids |
| TC12 | Extra: poison message to dead-letter queue |

## 2. Run the unit tests (no Docker needed)

```bash
go test ./... -cover
go test ./... -coverprofile=cover.out && go tool cover -html=cover.out   # coverage report for screenshots
```

Coverage of the services is low on purpose: most of their code needs a real database and broker, which TC01-TC11 cover instead. Say this in the report.

## 3. Screenshots to take (the runner cannot take these)

Put each next to its test row. Keep the terminals visible and readable.

- TC01: Playback terminal `[publish] type=track.played id=...`
- TC02/03: Analytics + Playlist terminals side by side showing the SAME event id
- TC04: Analytics `milestone reached` then Notification `created for user=all`
- TC05: `docker exec soundwave-rabbitmq rabbitmqctl list_queues name messages_ready` with `notification.queue` > 0, and the dashboard Queues tab
- TC06: same command showing 0, Notification terminal processing the backlog
- TC07/08: `duplicate event ignored` lines; the dashboard Publish dialog
- TC09: the UI after the workflow (Part F), plus the runner's terminal summary
- TC12: retries 1/3, 2/3, 3/3 then `failed 3 times -> DLQ`; dashboard showing `notification.queue.dlq` = 1
- Terminal with `go test ./... -cover`

## 4. Practical notes

- The tests create data (`e2e-...` users and tracks). Before the live demo reset: `docker compose down -v && docker compose up -d` and restart the services.
- Pre-flight fails if a service or queue is missing. Start consumers first so queues exist.
- If a test fails, do not edit the expected result. Copy the `[FAIL]` lines and the service log into a GitHub Issue for Jigden (TEAM_GUIDE rule 6).
- Work on a branch: `git checkout -b testing`, commit, push, open a Pull Request.

## 5. Things noticed while reading the code (report to Jigden)

1. Validation only checks `== ""`, so a title or playlist name made of spaces is accepted. The runner records this as a finding in TC10.
2. Analytics publishes `track.milestone_reached` before committing its database transaction. If the commit failed after a successful publish, the retry would publish a second milestone event with a new id, so Notification could create two notifications. Rare, not tested, and an honest limitation to mention (at-least-once delivery).
