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
	Revision   uint64 `json:"revision,omitempty"`
	Stream     string `json:"stream,omitempty"`
	Data       string `json:"data,omitempty"`
	Reset      bool   `json:"reset,omitempty"`
}

type Broker struct {
	mu       sync.Mutex
	clients  map[chan Event]struct{}
	revision uint64
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
	if _, ok := b.clients[ch]; ok {
		delete(b.clients, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *Broker) Publish(reason string) {
	b.PublishEvent(Event{Reason: reason})
}

func (b *Broker) Revision() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.revision
}

func (b *Broker) PublishEvent(event Event) {
	event.Reason = strings.TrimSpace(event.Reason)
	if event.Reason == "" {
		event.Reason = "changed"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Dashboard invalidations and job log fragments have independent cursors.
	if event.Reason != "job.log" {
		b.revision++
		event.Revision = b.revision
	}
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
			delete(b.clients, ch)
			close(ch)
		}
	}
}
