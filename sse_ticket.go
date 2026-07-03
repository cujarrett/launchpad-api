package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// ticketTTL is deliberately short — a ticket only needs to survive the moment
// between being minted and the browser opening the EventSource connection.
const ticketTTL = 30 * time.Second

type ticketEntry struct {
	roles     []string
	expiresAt time.Time
}

// ticketStore issues short-lived, single-use tickets so the SSE endpoint
// never sees a real, reusable Bearer token in a URL. EventSource can't send
// an Authorization header, so the frontend exchanges its real token for a
// ticket over a normal (header-authenticated) request first, then opens the
// stream with the ticket instead. A leaked ticket is worthless beyond that
// one connection attempt.
type ticketStore struct {
	mu      sync.Mutex
	tickets map[string]ticketEntry
}

func newTicketStore() *ticketStore {
	return &ticketStore{tickets: make(map[string]ticketEntry)}
}

func (s *ticketStore) mint(roles []string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	s.tickets[ticket] = ticketEntry{roles: roles, expiresAt: time.Now().Add(ticketTTL)}
	s.mu.Unlock()

	return ticket, nil
}

// redeem consumes a ticket — each ticket is valid for exactly one lookup.
func (s *ticketStore) redeem(ticket string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tickets[ticket]
	delete(s.tickets, ticket)
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.roles, true
}

// startSweeper periodically evicts unredeemed tickets so abandoned
// connection attempts don't grow the map forever.
func (s *ticketStore) startSweeper(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for k, v := range s.tickets {
				if now.After(v.expiresAt) {
					delete(s.tickets, k)
				}
			}
			s.mu.Unlock()
		}
	}
}
