package events

import (
	"strings"
	"sync"
)

type Event struct {
	Reason     string `json:"reason"`
	ServerName string `json:"server_name,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	Sequence   int64  `json:"sequence,omitempty"`
	Stream     string `json:"stream,omitempty"`
	Data       string `json:"data,omitempty"`
}

type Broker struct {
	mu      sync.Mutex
	clients map[chan Event]struct{}
}

func NewBroker() *Broker {
	return &Broker{clients: make(map[chan Event]struct{})}
}

func (b *Broker) Subscribe() chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.clients, ch)
	close(ch)
	b.mu.Unlock()
}

func (b *Broker) Publish(reason string) {
	b.PublishEvent(Event{Reason: reason})
}

func (b *Broker) PublishEvent(event Event) {
	event.Reason = strings.TrimSpace(event.Reason)
	if event.Reason == "" {
		event.Reason = "changed"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}
