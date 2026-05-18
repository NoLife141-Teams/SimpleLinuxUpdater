package events

import (
	"strings"
	"sync"
)

type Broker struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func NewBroker() *Broker {
	return &Broker{clients: make(map[chan string]struct{})}
}

func (b *Broker) Subscribe() chan string {
	ch := make(chan string, 8)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.clients, ch)
	close(ch)
	b.mu.Unlock()
}

func (b *Broker) Publish(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "changed"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- reason:
		default:
		}
	}
}
