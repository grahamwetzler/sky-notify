package app

import (
	"log/slog"
	"sync"
)

// queue is a bounded, key-deduplicated set of pending alerts. An entry stays present
// through delivery so the poll loop cannot re-enqueue a key that is in flight.
type queue struct {
	mu    sync.Mutex
	items map[string]*pendingAlert
	max   int
	wake  chan struct{}
}

type pendingAlert struct {
	alert    *Alert
	inflight bool
}

func newQueue(max int) *queue {
	return &queue{items: map[string]*pendingAlert{}, max: max, wake: make(chan struct{}, 1)}
}

func (q *queue) add(a *Alert) {
	key := cooldownKey(a.Hex, a.Trigger)
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, exists := q.items[key]; exists {
		return
	}
	if len(q.items) >= q.max {
		// An emergency evicts an ordinary alert rather than being dropped behind it.
		if !a.Emergency || !q.evictOrdinaryLocked() {
			slog.Warn("pending queue full, dropping alert", "icao", a.Hex, "trigger", a.Trigger)
			return
		}
	}
	q.items[key] = &pendingAlert{alert: a}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) evictOrdinaryLocked() bool {
	for k, p := range q.items {
		if !p.inflight && !p.alert.Emergency {
			delete(q.items, k)
			return true
		}
	}
	return false
}

// take returns the next alert to deliver, preferring emergencies. Map iteration order
// is random, so without this an urgent squawk could sit behind arbitrary routine traffic.
func (q *queue) take() *Alert {
	q.mu.Lock()
	defer q.mu.Unlock()
	var fallback *pendingAlert
	for _, p := range q.items {
		if p.inflight {
			continue
		}
		if p.alert.Emergency {
			p.inflight = true
			return p.alert
		}
		if fallback == nil {
			fallback = p
		}
	}
	if fallback != nil {
		fallback.inflight = true
		return fallback.alert
	}
	return nil
}

func (q *queue) done(key string) {
	q.mu.Lock()
	delete(q.items, key)
	q.mu.Unlock()
}

func (q *queue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
