package budget

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

// Without budget config the engine must not run a flusher, read the KV store or log, even when the
// service starts it.
func TestEngineWithoutPolicyDoesNothingEvenWhenStarted(t *testing.T) {
	hook := captureLogs(t)
	var kvCalls, tickers atomic.Int32
	e := New(
		WithKV(func() (kvstore.KVClient, bool, error) {
			kvCalls.Add(1)
			return nil, false, nil
		}),
		withTicker(func(time.Duration) (<-chan time.Time, func()) {
			tickers.Add(1)
			return make(chan time.Time), func() {}
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	e.Start(ctx)
	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	e.HandleUsage(ctx, rec(keyX, "m", 5))
	if d, ticket := e.Admit(keyX, "m"); d != (Decision{}) || ticket != nil {
		t.Fatalf("Admit() = %+v, %v, want the zero decision", d, ticket)
	}

	if got := tickers.Load(); got != 0 {
		t.Fatalf("a flusher ticker was created %d times with no policy installed", got)
	}
	if got := kvCalls.Load(); got != 0 {
		t.Fatalf("the KV store was looked up %d times with no policy installed", got)
	}
	if entries := hook.AllEntries(); len(entries) != 0 {
		t.Fatalf("the engine logged %d entries with no policy installed, first: %q", len(entries), entries[0].Message)
	}
}

type recordedTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
}

type tickerLog struct {
	mu   sync.Mutex
	made []*recordedTicker
}

func (l *tickerLog) factory(time.Duration) (<-chan time.Time, func()) {
	r := &recordedTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	l.mu.Lock()
	l.made = append(l.made, r)
	l.mu.Unlock()
	return r.ticks, func() { close(r.stopped) }
}

func (l *tickerLog) at(i int) *recordedTicker {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i >= len(l.made) {
		return nil
	}
	return l.made[i]
}

func (l *tickerLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.made)
}

func requireStopped(t *testing.T, r *recordedTicker, what string) {
	t.Helper()
	if r == nil {
		t.Fatalf("%s: no flusher was started", what)
	}
	select {
	case <-r.stopped:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: the flusher did not stop", what)
	}
}

func TestFlusherRunsOnlyWhileAPolicyIsInstalled(t *testing.T) {
	var tickers tickerLog
	e := New(staticKV(newFakeKV()), withTicker(tickers.factory))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := sdkConfig(budgetsFor("team-x", rl("*", "day", 100, "enforce")))

	e.Start(ctx)
	e.Start(ctx) // starting twice must not spawn a second flusher
	if tickers.count() != 0 {
		t.Fatal("Start with no policy must not start the flusher")
	}

	if err := e.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(cfg); err != nil { // a reload with budgets still configured keeps the same flusher
		t.Fatal(err)
	}
	if tickers.count() != 1 {
		t.Fatalf("Apply with budgets started %d flushers, want 1", tickers.count())
	}

	if err := e.Apply(nil); err != nil {
		t.Fatal(err)
	}
	requireStopped(t, tickers.at(0), "removing the budgets")

	if err := e.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if tickers.count() != 2 {
		t.Fatalf("adding budgets back started %d flushers in total, want 2", tickers.count())
	}
	cancel()
	requireStopped(t, tickers.at(1), "cancelling the service context")
}

func TestFlusherFlushesOnTick(t *testing.T) {
	var tickers tickerLog
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 100, "enforce")), withTicker(tickers.factory))
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 5))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.Start(ctx)
	r := tickers.at(0)
	if r == nil {
		t.Fatal("Start with a policy installed must start the flusher")
	}
	r.ticks <- time.Time{}
	r.ticks <- time.Time{} // the loop only receives this one after finishing the first flush
	if kv.total() != 5_000 {
		t.Fatalf("a tick must flush pending usage, total = %d", kv.total())
	}
}
