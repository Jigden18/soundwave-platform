package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"soundwave/pkg/events"
)

const maxRetries = 3

// Handler processes one event. Return an error to trigger retry / dead-lettering.
type Handler func(env events.Envelope) error

type Broker struct {
	conn *amqp.Connection
	pub  *amqp.Channel
	mu   sync.Mutex
}

// URL returns the broker URL from RABBITMQ_URL or a local default.
func URL() string {
	if u := os.Getenv("RABBITMQ_URL"); u != "" {
		return u
	}
	return "amqp://guest:guest@localhost:5672/"
}

// Connect retries until RabbitMQ is reachable, then declares the exchanges.
func Connect(url string) (*Broker, error) {
	var conn *amqp.Connection
	var err error
	for i := 1; i <= 10; i++ {
		conn, err = amqp.Dial(url)
		if err == nil {
			break
		}
		log.Printf("[broker] connect attempt %d/10 failed: %v", i, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return nil, err
	}

	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil { // publisher confirms
		return nil, err
	}
	if err := ch.ExchangeDeclare(events.ExchangeName, "topic", true, false, false, false, nil); err != nil {
		return nil, err
	}
	if err := ch.ExchangeDeclare(events.DLXExchangeName, "direct", true, false, false, false, nil); err != nil {
		return nil, err
	}
	log.Println("[broker] connected")
	return &Broker{conn: conn, pub: ch}, nil
}

func (b *Broker) Close() { b.conn.Close() }

// Publish sends a persistent event and waits for the broker to confirm it.
func (b *Broker) Publish(ctx context.Context, eventType string, data any) (events.Envelope, error) {
	env, err := events.NewEnvelope(eventType, data)
	if err != nil {
		return env, err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return env, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	dc, err := b.pub.PublishWithDeferredConfirmWithContext(ctx, events.ExchangeName, eventType, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    env.ID,
			Timestamp:    env.Timestamp,
			Body:         body,
		})
	if err != nil {
		return env, err
	}
	ok, err := dc.WaitContext(ctx)
	if err != nil || !ok {
		return env, fmt.Errorf("publish not confirmed: %v", err)
	}
	log.Printf("[publish] type=%s id=%s", env.Type, env.ID)
	return env, nil
}

// Consume declares a durable queue (+ its DLQ), binds it to the routing keys,
// and processes messages in a background goroutine.
func (b *Broker) Consume(queue string, keys []string, h Handler) error {
	ch, err := b.conn.Channel()
	if err != nil {
		return err
	}

	dlq := queue + ".dlq"
	if _, err := ch.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(dlq, queue, events.DLXExchangeName, false, nil); err != nil {
		return err
	}

	args := amqp.Table{
		"x-dead-letter-exchange":    events.DLXExchangeName,
		"x-dead-letter-routing-key": queue,
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, args); err != nil {
		return err
	}
	for _, k := range keys {
		if err := ch.QueueBind(queue, k, events.ExchangeName, false, nil); err != nil {
			return err
		}
	}
	if err := ch.Qos(10, 0, false); err != nil {
		return err
	}

	msgs, err := ch.Consume(queue, "", false, false, false, false, nil) // manual acks
	if err != nil {
		return err
	}
	log.Printf("[consume] queue=%s keys=%v ready", queue, keys)

	go func() {
		for d := range msgs {
			handle(ch, queue, d, h)
		}
		log.Printf("[consume] queue=%s channel closed", queue)
	}()
	return nil
}

func handle(ch *amqp.Channel, queue string, d amqp.Delivery, h Handler) {
	var env events.Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		log.Printf("[consume] queue=%s malformed message -> DLQ: %v", queue, err)
		d.Reject(false)
		return
	}

	err := h(env)
	if err == nil {
		d.Ack(false)
		return
	}

	retries := retryCount(d.Headers)
	if retries >= maxRetries {
		log.Printf("[consume] queue=%s id=%s failed %d times -> DLQ: %v", queue, env.ID, retries, err)
		d.Reject(false) // goes to the dead-letter exchange
		return
	}

	log.Printf("[consume] queue=%s id=%s error (retry %d/%d): %v", queue, env.ID, retries+1, maxRetries, err)
	time.Sleep(2 * time.Second) // simple backoff

	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers["x-retry-count"] = int32(retries + 1)

	// Republish straight to this queue, then ack the original.
	pubErr := ch.PublishWithContext(context.Background(), "", queue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    d.MessageId,
		Timestamp:    d.Timestamp,
		Headers:      headers,
		Body:         d.Body,
	})
	if pubErr != nil {
		d.Nack(false, true) // could not republish: requeue the original
		return
	}
	d.Ack(false)
}

func retryCount(h amqp.Table) int {
	switch v := h["x-retry-count"].(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	}
	return 0
}