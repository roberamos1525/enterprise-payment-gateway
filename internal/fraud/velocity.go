package fraud

import (
	"sync"
	"time"
)

// Velocity counts events per key in a sliding time window. It keeps a
// timestamp log per key, pruned on access and by Sweep. State is
// in-process: counts reset on restart and are not shared across instances.
type Velocity struct {
	mu      sync.Mutex
	window  time.Duration
	maxKeys int
	events  map[string][]time.Time
}

// NewVelocity builds a counter for the given window. maxKeys bounds memory;
// when exceeded, Sweep is forced and, if still full, the hit is not stored.
func NewVelocity(window time.Duration, maxKeys int) *Velocity {
	if maxKeys <= 0 {
		maxKeys = 100_000
	}
	return &Velocity{window: window, maxKeys: maxKeys, events: map[string][]time.Time{}}
}

// Hit records an event for key at now and returns the number of events in
// the window, including this one.
func (v *Velocity) Hit(key string, now time.Time) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	log := prune(v.events[key], now.Add(-v.window))
	if _, exists := v.events[key]; !exists && len(v.events) >= v.maxKeys {
		v.sweepLocked(now)
		if len(v.events) >= v.maxKeys {
			return len(log) + 1
		}
	}
	log = append(log, now)
	v.events[key] = log
	return len(log)
}

// Count returns the number of events for key in the window without recording.
func (v *Velocity) Count(key string, now time.Time) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	log := prune(v.events[key], now.Add(-v.window))
	if len(log) == 0 {
		delete(v.events, key)
	} else {
		v.events[key] = log
	}
	return len(log)
}

// Sweep drops expired events and empty keys; it returns the live key count.
func (v *Velocity) Sweep(now time.Time) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sweepLocked(now)
	return len(v.events)
}

func (v *Velocity) sweepLocked(now time.Time) {
	cutoff := now.Add(-v.window)
	for k, log := range v.events {
		log = prune(log, cutoff)
		if len(log) == 0 {
			delete(v.events, k)
		} else {
			v.events[k] = log
		}
	}
}

// prune removes timestamps at or before cutoff (logs are append-ordered).
func prune(log []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(log) && !log[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return log
	}
	return append(log[:0], log[i:]...)
}
