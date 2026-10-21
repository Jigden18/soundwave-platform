package broker

import (
	"encoding/json"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"soundwave/pkg/events"
)

// spyAcker is a test double (a spy): it records how the message was acknowledged.
type spyAcker struct {
	acks, rejects, nacks int
	lastRequeue          bool
}

func (s *spyAcker) Ack(uint64, bool) error { s.acks++; return nil }
func (s *spyAcker) Nack(_ uint64, _ bool, requeue bool) error {
	s.nacks++
	s.lastRequeue = requeue
	return nil
}
func (s *spyAcker) Reject(_ uint64, requeue bool) error {
	s.rejects++
	s.lastRequeue = requeue
	return nil
}

func delivery(s *spyAcker, body []byte, headers amqp.Table) amqp.Delivery {
	return amqp.Delivery{Acknowledger: s, Body: body, Headers: headers}
}

func validBody(t *testing.T) []byte {
	env, err := events.NewEnvelope(events.TrackPlayed, events.TrackPlayedData{UserID: "u1", TrackID: "t1", Title: "S"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

func TestRetryCount(t *testing.T) {
	cases := []struct {
		name string
		h    amqp.Table
		want int
	}{
		{"no headers", nil, 0},
		{"header absent", amqp.Table{"other": 1}, 0},
		{"int32", amqp.Table{"x-retry-count": int32(2)}, 2},
		{"int64", amqp.Table{"x-retry-count": int64(3)}, 3},
		{"int", amqp.Table{"x-retry-count": 1}, 1},
		{"wrong type is treated as zero", amqp.Table{"x-retry-count": "2"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryCount(c.h); got != c.want {
				t.Errorf("retryCount = %d, want %d", got, c.want)
			}
		})
	}
}

func TestURL_DefaultAndOverride(t *testing.T) {
	t.Setenv("RABBITMQ_URL", "")
	if got := URL(); got != "amqp://guest:guest@localhost:5672/" {
		t.Errorf("default URL = %q", got)
	}
	t.Setenv("RABBITMQ_URL", "amqp://u:p@broker:5672/")
	if got := URL(); got != "amqp://u:p@broker:5672/" {
		t.Errorf("override URL = %q", got)
	}
}

func TestHandle_SuccessfulHandlerAcks(t *testing.T) {
	spy := &spyAcker{}
	called := false
	handle(nil, "q", delivery(spy, validBody(t), nil), func(events.Envelope) error { called = true; return nil })
	if !called {
		t.Error("handler was not called")
	}
	if spy.acks != 1 || spy.rejects != 0 || spy.nacks != 0 {
		t.Errorf("want exactly one ack, got %+v", spy)
	}
}

func TestHandle_MalformedMessageGoesToDLQWithoutHandler(t *testing.T) {
	spy := &spyAcker{}
	called := false
	handle(nil, "q", delivery(spy, []byte("not json"), nil), func(events.Envelope) error { called = true; return nil })
	if called {
		t.Error("handler must not run for a malformed message")
	}
	if spy.rejects != 1 || spy.lastRequeue {
		t.Errorf("want one reject with requeue=false (dead-letter), got %+v", spy)
	}
}

func TestHandle_FailureAfterMaxRetriesIsDeadLettered(t *testing.T) {
	spy := &spyAcker{}
	h := amqp.Table{"x-retry-count": int32(maxRetries)}
	handle(nil, "q", delivery(spy, validBody(t), h), func(events.Envelope) error { return errors.New("boom") })
	if spy.rejects != 1 || spy.lastRequeue || spy.acks != 0 {
		t.Errorf("want one reject with requeue=false and no ack, got %+v", spy)
	}
}
