package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ChannelBus is a channel-based Bus implementation with per-subscriber
// buffered channels, fan-out delivery, and drop-oldest backpressure.
//
// Each subscriber gets its own goroutine and buffered channel. The Publish
// method never blocks: if a subscriber's buffer is full, the oldest event
// is evicted to make room for the new one.
type ChannelBus struct {
	cfg     BusConfig
	metrics BusMetrics
	logger  *slog.Logger

	mu     sync.RWMutex
	topics map[Topic]*topicEntry
	closed atomic.Bool
	wg     sync.WaitGroup // tracks subscriber goroutines
}

var _ Bus = (*ChannelBus)(nil)

// NewChannelBus creates a ChannelBus. Zero-value fields in cfg are replaced
// with DefaultBusConfig defaults.
func NewChannelBus(cfg BusConfig, metrics BusMetrics, logger *slog.Logger) *ChannelBus {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = DefaultBusConfig().BufferSize
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultBusConfig().DrainTimeout
	}
	return &ChannelBus{
		cfg:     cfg,
		metrics: metrics,
		logger:  logger,
		topics:  make(map[Topic]*topicEntry),
	}
}

// Publish sends an event to all subscribers of its topic. It never blocks
// on slow subscribers. Returns ErrBusClosed after shutdown. The context
// can cancel a publish mid-fan-out.
func (b *ChannelBus) Publish(ctx context.Context, event Event) error {
	if b.closed.Load() {
		return ErrBusClosed
	}

	start := time.Now()
	topic := event.Topic
	b.metrics.IncPublished(topic)

	te := getTopicEntry(b.topics, &b.mu, topic)
	if te == nil {
		b.metrics.ObservePublishDuration(topic, time.Since(start).Seconds())
		return nil
	}

	te.mu.RLock()
	subs := make([]*subscriber, 0, len(te.subscribers))
	for _, s := range te.subscribers {
		subs = append(subs, s)
	}
	te.mu.RUnlock()

	for _, s := range subs {
		select {
		case <-ctx.Done():
			b.metrics.ObservePublishDuration(topic, time.Since(start).Seconds())
			return fmt.Errorf("publish(%s): %w", topic, ctx.Err())
		default:
		}
		sendToSubscriber(s, event, b.metrics, b.logger)
	}

	b.metrics.ObservePublishDuration(topic, time.Since(start).Seconds())
	return nil
}

// Subscribe registers a handler for a topic. The handler runs in a
// dedicated goroutine. Returns ErrBusClosed after shutdown.
func (b *ChannelBus) Subscribe(topic Topic, handler Handler) (Subscription, error) {
	if b.closed.Load() {
		return Subscription{}, ErrBusClosed
	}

	sub := &subscriber{
		id:      generateID(),
		ch:      make(chan Event, b.cfg.BufferSize),
		handler: handler,
		done:    make(chan struct{}),
	}

	te := getOrCreateTopicEntry(b.topics, &b.mu, topic)
	te.mu.Lock()
	te.subscribers[sub.id] = sub
	count := len(te.subscribers)
	te.mu.Unlock()

	b.metrics.SetSubscriberCount(topic, count)
	b.logger.Debug("subscriber registered",
		slog.String("subscriber_id", sub.id),
		slog.String("topic", string(topic)),
		slog.Int("subscriber_count", count),
	)

	b.wg.Add(1)
	go deliverLoop(sub, topic, b.metrics, &b.wg)

	return Subscription{ID: sub.id, Topic: topic}, nil
}

// Unsubscribe removes a subscription and stops its delivery goroutine.
func (b *ChannelBus) Unsubscribe(sub Subscription) error {
	te := getTopicEntry(b.topics, &b.mu, sub.Topic)
	if te == nil {
		return fmt.Errorf("unsubscribe(%s): %w", sub.ID, ErrSubscriptionNotFound)
	}

	te.mu.Lock()
	s, ok := te.subscribers[sub.ID]
	if !ok {
		te.mu.Unlock()
		return fmt.Errorf("unsubscribe(%s): %w", sub.ID, ErrSubscriptionNotFound)
	}
	delete(te.subscribers, sub.ID)
	count := len(te.subscribers)
	te.mu.Unlock()

	b.metrics.SetSubscriberCount(sub.Topic, count)
	close(s.done)

	b.logger.Debug("subscriber unsubscribed",
		slog.String("subscriber_id", sub.ID),
		slog.String("topic", string(sub.Topic)),
		slog.Int("subscriber_count", count),
	)
	return nil
}

// Close gracefully shuts down the bus: stops accepting publishes, signals
// all subscriber goroutines to drain, and waits up to the context deadline
// or DrainTimeout.
func (b *ChannelBus) Close(ctx context.Context) error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.logger.Info("event bus shutting down, draining pending events")

	// Deregister as we collect. Close signals every subscriber by closing its
	// done channel, and leaving them registered would let a consumer that
	// unsubscribes itself on shutdown close the SAME channel a second time —
	// a panic, during shutdown, in whichever consumer happened to stop last.
	// Dropping them here turns that into the ErrSubscriptionNotFound every
	// Unsubscribe caller already handles (MYR-410).
	b.mu.RLock()
	var allSubs []*subscriber
	for _, te := range b.topics {
		te.mu.Lock()
		for id, s := range te.subscribers {
			allSubs = append(allSubs, s)
			delete(te.subscribers, id)
		}
		te.mu.Unlock()
	}
	b.mu.RUnlock()

	for _, s := range allSubs {
		close(s.done)
	}

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	drainCtx, cancel := context.WithTimeout(ctx, b.cfg.DrainTimeout)
	defer cancel()

	select {
	case <-done:
		b.logger.Info("event bus drained and stopped cleanly")
	case <-drainCtx.Done():
		b.logger.Warn("event bus drain timed out, some events may have been lost")
	}
	return nil
}
