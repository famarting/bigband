package events_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/famarting/bigband/internal/events"
)

func newTestBus(t *testing.T) *events.Bus {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	bus, err := events.NewBus(path)
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

func TestPublishSubscribeRoundtrip(t *testing.T) {
	bus := newTestBus(t)
	ch, cancel := bus.Subscribe(events.Filter{}, "test")
	defer cancel()

	env := events.Envelope{
		Type:     events.TypeTaskRunStarted,
		TaskName: "my-task",
		RunID:    "my-task/2024-01-01T00-00-00Z",
	}
	bus.Publish(env)

	select {
	case got := <-ch:
		if got.Type != events.TypeTaskRunStarted {
			t.Errorf("want type %s, got %s", events.TypeTaskRunStarted, got.Type)
		}
		if got.TaskName != "my-task" {
			t.Errorf("want task my-task, got %s", got.TaskName)
		}
		if got.EventID == "" {
			t.Error("EventID should be auto-populated")
		}
		if got.OccurredAt.IsZero() {
			t.Error("OccurredAt should be auto-populated")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestSubscribeFilter_Type(t *testing.T) {
	bus := newTestBus(t)
	ch, cancel := bus.Subscribe(events.Filter{Types: []events.Type{events.TypeTaskRunCompleted}}, "filtered")
	defer cancel()

	// Publish a non-matching event first.
	bus.Publish(events.Envelope{Type: events.TypeClaudeSessionStarted, TaskName: "t1"})
	// Publish a matching event.
	bus.Publish(events.Envelope{Type: events.TypeTaskRunCompleted, TaskName: "t2"})

	select {
	case got := <-ch:
		if got.Type != events.TypeTaskRunCompleted {
			t.Errorf("want task_run.completed, got %s", got.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for filtered event")
	}
	// Channel should be empty — the non-matching one was dropped.
	select {
	case extra := <-ch:
		t.Errorf("unexpected event: %s", extra.Type)
	default:
	}
}

func TestSubscribeFilter_Task(t *testing.T) {
	bus := newTestBus(t)
	ch, cancel := bus.Subscribe(events.Filter{Tasks: []string{"target-task"}}, "task-filtered")
	defer cancel()

	bus.Publish(events.Envelope{Type: events.TypeTaskRunStarted, TaskName: "other-task"})
	bus.Publish(events.Envelope{Type: events.TypeTaskRunStarted, TaskName: "target-task"})

	select {
	case got := <-ch:
		if got.TaskName != "target-task" {
			t.Errorf("want target-task, got %s", got.TaskName)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
	select {
	case extra := <-ch:
		t.Errorf("unexpected event for task %s", extra.TaskName)
	default:
	}
}

func TestEventIDGeneration(t *testing.T) {
	bus := newTestBus(t)
	ch, cancel := bus.Subscribe(events.Filter{}, "id-gen")
	defer cancel()

	const n = 100
	for i := 0; i < n; i++ {
		bus.Publish(events.Envelope{Type: events.TypeTaskRunStarted})
	}

	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		select {
		case env := <-ch:
			if seen[env.EventID] {
				t.Errorf("duplicate event ID: %s", env.EventID)
			}
			seen[env.EventID] = true
		case <-time.After(time.Second):
			t.Fatalf("timeout after %d events", i)
		}
	}
}

func TestBusFileAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	bus, err := events.NewBus(path)
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}

	for i := 0; i < 5; i++ {
		bus.Publish(events.Envelope{Type: events.TypeTaskRunStarted, TaskName: "task"})
	}
	bus.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := splitLines(data)
	if len(lines) != 5 {
		t.Errorf("want 5 lines, got %d", len(lines))
	}
	for i, line := range lines {
		var env events.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Errorf("line %d: invalid JSON: %v", i, err)
		}
	}
}

func TestConcurrentPublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	bus, err := events.NewBus(path)
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	const (
		goroutines  = 20
		eventsEach  = 50
		totalEvents = goroutines * eventsEach
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < eventsEach; j++ {
				bus.Publish(events.Envelope{Type: events.TypeTaskRunStarted})
			}
		}()
	}
	wg.Wait()
	bus.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := splitLines(data)
	if len(lines) != totalEvents {
		t.Errorf("want %d lines, got %d", totalEvents, len(lines))
	}
}

func TestBusClose_SubscriberChannelClosed(t *testing.T) {
	bus := newTestBus(t)
	ch, _ := bus.Subscribe(events.Filter{}, "close-test")
	bus.Close()
	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel, got a value")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for channel close")
	}
}

func TestBusClose_PublishNoPanic(t *testing.T) {
	bus := newTestBus(t)
	bus.Close()
	// Should not panic.
	bus.Publish(events.Envelope{Type: events.TypeTaskRunStarted})
}

func TestLaggedSubscriberIsolation(t *testing.T) {
	bus := newTestBus(t)

	// Slow subscriber: never reads — will lag.
	_, slowCancel := bus.Subscribe(events.Filter{}, "slow")
	defer slowCancel()

	// Fast subscriber.
	fastCh, fastCancel := bus.Subscribe(events.Filter{}, "fast")
	defer fastCancel()

	// Publish more events than the subscriber buffer (1024) — enough to lag
	// the slow subscriber. Publishing 1100 events; slow never reads.
	const n = 1100
	for i := 0; i < n; i++ {
		bus.Publish(events.Envelope{Type: events.TypeTaskRunStarted})
	}

	// Fast subscriber should have received events (drain its buffer).
	received := 0
	drain:
	for {
		select {
		case <-fastCh:
			received++
		default:
			break drain
		}
	}
	if received == 0 {
		t.Error("fast subscriber should have received events despite slow subscriber lag")
	}
}

// splitLines splits a byte slice into non-empty lines.
func splitLines(data []byte) []string {
	var lines []string
	start := 0
	for i, b := range data {
		if b == '\n' {
			line := string(data[start:i])
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(data) {
		line := string(data[start:])
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
