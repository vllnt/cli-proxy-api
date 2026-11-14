package budget

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

const loadTestWait = 5 * time.Second

// newAsyncEngine returns an engine that loads persisted totals the way production does, off the
// calling goroutine, over a fake store.
func newAsyncEngine(t *testing.T) (*Engine, *fakeKV) {
	t.Helper()
	clock := newFakeClock(saturdayNoon)
	kv := newFakeKV()
	e := New(WithClock(clock.Now), staticKV(kv))
	if err := e.Apply(sdkConfig(budgetsFor("team-x", rl("*", "day", 100, "enforce")))); err != nil {
		t.Fatal(err)
	}
	return e, kv
}

// dayCounterKey is the KV key of the "*" day rule of team-x on saturdayNoon.
func dayCounterKey() string {
	return "budget:v1:" + kvstore.HashKeyPart("team-x") + ":" + ruleID("*", "day") + ":" + fmt.Sprint(utc("2026-11-14T00:00:00Z").Unix())
}

// blockGets makes every KVGet wait until the returned release func is called.
func (f *fakeKV) blockGets() (release func(), started <-chan struct{}) {
	gate, signal := make(chan struct{}), make(chan struct{}, 16)
	f.mu.Lock()
	f.getGate, f.getStarted = gate, signal
	f.mu.Unlock()
	var released bool
	return func() {
		if !released {
			released = true
			close(gate)
		}
	}, signal
}

func (f *fakeKV) setGetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *fakeKV) gets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

func (f *fakeKV) put(key string, milli int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = milli
}

// requireReturns fails the test when fn does not return promptly.
func requireReturns(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s blocked on the KV store", what)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(loadTestWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func remaining(e *Engine) int64 {
	d, _ := e.Admit(keyX, "m")
	return d.RemainingTokens
}

// A stalled store must never stall a request: neither admission nor charging may wait for the
// persisted total to load.
func TestAdmitDoesNotWaitForTheCounterLoad(t *testing.T) {
	e, kv := newAsyncEngine(t)
	kv.put(dayCounterKey(), 7_000)
	release, started := kv.blockGets()
	t.Cleanup(release)

	var ticket *Ticket
	requireReturns(t, "Admit", func() { _, ticket = e.Admit(keyX, "m") })
	<-started
	requireReturns(t, "a second Admit", func() { _, ticket = e.Admit(keyX, "m") })
	requireReturns(t, "HandleUsage", func() { charge(e, ticket, rec(keyX, "m", 5)) })
	if got := remaining(e); got != 95 {
		t.Fatalf("remaining while the load is pending = %d, want 95 (in-memory usage only)", got)
	}

	release()
	waitUntil(t, "the persisted total to arrive", func() bool { return remaining(e) == 88 })
}

func TestChargeWithoutATicketDoesNotWaitForTheCounterLoad(t *testing.T) {
	e, kv := newAsyncEngine(t)
	kv.put(dayCounterKey(), 7_000)
	release, started := kv.blockGets()
	t.Cleanup(release)

	requireReturns(t, "HandleUsage", func() { e.HandleUsage(context.Background(), rec(keyX, "m", 5)) })
	<-started
	if got := remaining(e); got != 95 {
		t.Fatalf("remaining while the load is pending = %d, want 95", got)
	}
	release()
	waitUntil(t, "the persisted total to arrive", func() bool { return remaining(e) == 88 })
}

// Usage charged while the load is pending must neither be lost nor counted twice, and must not reach
// the store before the persisted total is known.
func TestPersistedTotalIsAddedOnceAndKeepsUsageChargedWhileLoading(t *testing.T) {
	e, kv := newAsyncEngine(t)
	kv.put(dayCounterKey(), 7_000)
	release, started := kv.blockGets()
	t.Cleanup(release)

	var ticket *Ticket
	requireReturns(t, "Admit", func() { _, ticket = e.Admit(keyX, "m") })
	<-started
	charge(e, ticket, rec(keyX, "m", 5))
	if err := e.flush(context.Background(), false); err != nil {
		t.Fatalf("flush() = %v", err)
	}
	if kv.incrCalls != 0 || kv.total() != 7_000 {
		t.Fatalf("usage was written before the persisted total loaded: incr=%d total=%d", kv.incrCalls, kv.total())
	}

	release()
	waitUntil(t, "the persisted total to arrive", func() bool { return remaining(e) == 88 })
	for i := 0; i < 3; i++ {
		if err := e.flush(context.Background(), false); err != nil {
			t.Fatalf("flush() = %v", err)
		}
	}
	if kv.total() != 12_000 {
		t.Fatalf("stored total = %d, want 12000 (persisted 7000 + 5000 charged, each once)", kv.total())
	}
	if got := remaining(e); got != 88 {
		t.Fatalf("remaining after flushing = %d, want 88", got)
	}
}

func TestFailedLoadIsRetriedByTheNextFlushAndAddsThePersistedTotalOnce(t *testing.T) {
	e, kv := newAsyncEngine(t)
	kv.put(dayCounterKey(), 7_000)
	kv.setGetErr(errors.New("kv down"))

	_, ticket := e.Admit(keyX, "m")
	waitUntil(t, "the first load attempt", func() bool { return kv.gets() >= 1 })
	charge(e, ticket, rec(keyX, "m", 5))
	if err := e.flush(context.Background(), false); err != nil {
		t.Fatalf("flush() = %v", err)
	}
	if kv.incrCalls != 0 {
		t.Fatalf("usage was written while the persisted total is unknown: incr=%d", kv.incrCalls)
	}

	kv.setGetErr(nil)
	waitUntil(t, "a flush to retry the load and persist the usage", func() bool {
		_ = e.flush(context.Background(), false)
		return kv.total() == 12_000
	})
	for i := 0; i < 3; i++ {
		if err := e.flush(context.Background(), false); err != nil {
			t.Fatalf("flush() = %v", err)
		}
	}
	if kv.total() != 12_000 {
		t.Fatalf("stored total = %d, want 12000", kv.total())
	}
	if got := remaining(e); got != 88 {
		t.Fatalf("remaining = %d, want 88: the persisted total must be added exactly once", got)
	}
}

// TestApplyPreloadsCurrentWindowsSoTheFirstRequestAfterRestartSeesThePersistedTotal covers a restart:
// the persisted total must be loaded when the policy is applied, not lazily on the first request, which
// would admit that request against an empty counter.
func TestApplyPreloadsCurrentWindowsSoTheFirstRequestAfterRestartSeesThePersistedTotal(t *testing.T) {
	kv := newFakeKV()
	kv.put(dayCounterKey(), 100*milliPerToken)
	var queued []func()
	clock := newFakeClock(saturdayNoon)
	e := New(WithClock(clock.Now), staticKV(kv), func(e *Engine) { e.spawn = func(f func()) { queued = append(queued, f) } })
	if err := e.Apply(sdkConfig(budgetsFor("team-x", rl("*", "day", 100, "enforce")))); err != nil {
		t.Fatal(err)
	}
	for _, load := range queued {
		load()
	}
	if d, _ := e.Admit(keyX, "claude-fable-5"); d.Exhausted == nil {
		t.Fatalf("first request after restart was admitted against an unloaded counter: %+v", d)
	}
}
