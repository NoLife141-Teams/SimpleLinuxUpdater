package events

import (
	"testing"
	"time"
)

func TestBrokerPublishDeliversToSubscriber(t *testing.T) {
	broker := NewBroker()
	ch := broker.Subscribe()
	defer broker.Unsubscribe(ch)

	broker.Publish("updated")

	select {
	case got := <-ch:
		if got.Reason != "updated" {
			t.Fatalf("published reason = %q, want updated", got.Reason)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for published event")
	}
}

func TestBrokerPublishDefaultsBlankReason(t *testing.T) {
	broker := NewBroker()
	ch := broker.Subscribe()
	defer broker.Unsubscribe(ch)

	broker.Publish(" \n\t ")

	select {
	case got := <-ch:
		if got.Reason != "changed" {
			t.Fatalf("blank published reason = %q, want changed", got.Reason)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for default event")
	}
}

func TestBrokerPublishDoesNotBlockFullSubscriber(t *testing.T) {
	broker := NewBroker()
	ch := broker.Subscribe()
	defer broker.Unsubscribe(ch)

	for i := 0; i < cap(ch); i++ {
		broker.Publish("fill")
	}

	done := make(chan struct{})
	go func() {
		broker.Publish("dropped")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("publish blocked on full subscriber channel")
	}
}

func TestBrokerUnsubscribeClosesSubscriber(t *testing.T) {
	broker := NewBroker()
	ch := broker.Subscribe()

	broker.Unsubscribe(ch)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("unsubscribed channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for unsubscribe close")
	}
}

func TestBrokerPublishDeliversToMultipleSubscribers(t *testing.T) {
	broker := NewBroker()
	first := broker.Subscribe()
	defer broker.Unsubscribe(first)
	second := broker.Subscribe()
	defer broker.Unsubscribe(second)

	broker.Publish("refresh")

	for name, ch := range map[string]chan Event{"first": first, "second": second} {
		select {
		case got := <-ch:
			if got.Reason != "refresh" {
				t.Fatalf("%s subscriber reason = %q, want refresh", name, got.Reason)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s subscriber", name)
		}
	}
}

func TestBrokerPublishEventPreservesStructuredJobLog(t *testing.T) {
	broker := NewBroker()
	ch := broker.Subscribe()
	defer broker.Unsubscribe(ch)

	want := Event{
		Reason:     "job.log",
		ServerName: "alpha",
		JobID:      "job-1",
		Sequence:   7,
		Stream:     "stderr",
		Data:       "warning\n",
	}
	broker.PublishEvent(want)

	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("published event = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for structured event")
	}
}
