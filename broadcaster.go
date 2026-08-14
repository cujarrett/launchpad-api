package main

import "sync"

// broadcaster is a simple in-memory fan-out for ResourceStatus events.
// Each SSE client gets its own buffered channel.
// It also caches the last known status per resource so new clients
// get an immediate snapshot when they connect.
type broadcaster struct {
	mu      sync.RWMutex
	clients map[chan ResourceStatus]struct{}
	cache   map[string]ResourceStatus // key: tenant/kind/name
}

func newBroadcaster() *broadcaster {
	return &broadcaster{
		clients: make(map[chan ResourceStatus]struct{}),
		cache:   make(map[string]ResourceStatus),
	}
}

func (b *broadcaster) subscribe() chan ResourceStatus {
	ch := make(chan ResourceStatus, 64)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	// Replay cached state so the client gets an immediate snapshot.
	for _, s := range b.cache {
		select {
		case ch <- s:
		default:
		}
	}
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan ResourceStatus) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *broadcaster) publish(s ResourceStatus) {
	key := s.Workspace + "/" + s.Kind + "/" + s.Name
	b.mu.Lock()
	b.cache[key] = s
	for ch := range b.clients {
		select {
		case ch <- s:
		default: // slow client - drop
		}
	}
	b.mu.Unlock()
}

func (b *broadcaster) evict(workspace, kind, name string) {
	key := workspace + "/" + kind + "/" + name
	b.mu.Lock()
	delete(b.cache, key)
	b.mu.Unlock()
}
