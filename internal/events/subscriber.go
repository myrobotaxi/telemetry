package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// topicEntry holds all subscribers for a single topic.
type topicEntry struct {
	mu          sync.RWMutex
	subscribers map[string]*subscriber
}

// subscriber represents a single subscription: a handler running in a
// dedicated goroutine fed by a buffered channel.
type subscriber struct {
	id      string
	ch      chan Event
	handler Handler
	done    chan struct{} // closed to signal the goroutine to stop
}

// sendToSubscriber performs a non-blocking send. If the buffer is full, it
// evicts the oldest event (drop-oldest backpressure) before retrying.
func sendToSubscriber(s *subscriber, event Event, metrics BusMetrics, logger *slog.Logger) {
	select {
	case s.ch <- event:
		return
	default:
	}
	// Buffer full — drop oldest.
	select {
	case <-s.ch:
		metrics.IncDropped(event.Topic)
		logger.Warn("dropped oldest event for slow subscriber",
			slog.String("subscriber_id", s.id),
			slog.String("topic", string(event.Topic)),
			slog.String("event_id", event.ID),
		)
	default:
		// drained concurrently
	}
	select {
	case s.ch <- event:
	default:
		metrics.IncDropped(event.Topic)
	}
}

// deliverLoop reads events from the subscriber's channel and calls the
// handler until the done channel is closed, then drains remaining events.
func deliverLoop(s *subscriber, topic Topic, metrics BusMetrics, wg *sync.WaitGroup, live *atomic.Int64) {
	defer wg.Done()
	// live mirrors wg readably so a timed-out Close can report how many
	// delivery goroutines are still running (MYR-410).
	defer live.Add(-1)
	for {
		select {
		case <-s.done:
			drainSubscriber(s, topic, metrics)
			return
		case event, ok := <-s.ch:
			if !ok {
				return
			}
			s.handler(event)
			metrics.IncDelivered(topic)
		}
	}
}

// drainSubscriber delivers remaining buffered events after the stop signal.
func drainSubscriber(s *subscriber, topic Topic, metrics BusMetrics) {
	for {
		select {
		case event, ok := <-s.ch:
			if !ok {
				return
			}
			s.handler(event)
			metrics.IncDelivered(topic)
		default:
			return
		}
	}
}

// getTopicEntry returns the topicEntry for a topic, or nil if none exists.
func getTopicEntry(topics map[Topic]*topicEntry, mu *sync.RWMutex, topic Topic) *topicEntry {
	mu.RLock()
	defer mu.RUnlock()
	return topics[topic]
}

// getOrCreateTopicEntry returns the topicEntry for a topic, creating it if
// needed. Uses double-checked locking to minimise write-lock contention.
func getOrCreateTopicEntry(topics map[Topic]*topicEntry, mu *sync.RWMutex, topic Topic) *topicEntry {
	mu.RLock()
	te, ok := topics[topic]
	mu.RUnlock()
	if ok {
		return te
	}

	mu.Lock()
	defer mu.Unlock()
	if te, ok = topics[topic]; ok {
		return te
	}
	te = &topicEntry{subscribers: make(map[string]*subscriber)}
	topics[topic] = te
	return te
}
