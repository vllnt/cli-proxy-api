package usage

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type gatedPlugin struct {
	gate      chan struct{}
	delivered atomic.Int32
}

func (p *gatedPlugin) HandleUsage(context.Context, Record) {
	<-p.gate
	p.delivered.Add(1)
}

func waitStoppedWithin(m *Manager, d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return m.WaitStopped(ctx)
}

func TestWaitStoppedReturnsOnceQueuedRecordsAreDelivered(t *testing.T) {
	m := NewManager(4)
	plugin := &gatedPlugin{gate: make(chan struct{})}
	m.Register(plugin)
	m.Publish(context.Background(), Record{Model: "m"})
	m.Publish(context.Background(), Record{Model: "m"})
	m.Stop()

	if waitStoppedWithin(m, 20*time.Millisecond) {
		t.Fatal("WaitStopped reported a drained queue while a plugin still holds a record")
	}
	close(plugin.gate)
	if !waitStoppedWithin(m, 5*time.Second) {
		t.Fatal("WaitStopped did not return after the plugin was released")
	}
	if got := plugin.delivered.Load(); got != 2 {
		t.Fatalf("delivered %d records before the dispatcher stopped, want 2", got)
	}
}

func TestWaitStoppedWithoutAStartedDispatcherReturnsAtOnce(t *testing.T) {
	m := NewManager(1)
	m.Stop()
	if !waitStoppedWithin(m, time.Second) {
		t.Fatal("a manager that never started has nothing to wait for")
	}
	var nilManager *Manager
	if !nilManager.WaitStopped(context.Background()) {
		t.Fatal("a nil manager has nothing to wait for")
	}
}
