package budget

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func benchEngine(b *testing.B) (*Engine, *Ticket, usage.Record) {
	b.Helper()
	store, err := kvstore.OpenBoltStore(filepath.Join(b.TempDir(), "kv.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	clock := newFakeClock(saturdayNoon)
	e := New(WithClock(clock.Now), staticKV(store), withInlineLoads())
	budgets := budgetsFor("team-x",
		rl("claude-fable-*", "week", 1<<40, "enforce"),
		rl("*", "day", 1<<40, "enforce"),
	)
	if err := e.Apply(sdkConfig(budgets)); err != nil {
		b.Fatal(err)
	}
	_, ticket := e.Admit(keyX, "claude-fable-5")
	return e, ticket, rec(keyX, "claude-fable-5", 100)
}

// BenchmarkCharge measures what one usage record costs against the real BoltStore. Writes are batched,
// and the final Flush is inside the timed section, so the cost is amortised, not hidden.
func BenchmarkCharge(b *testing.B) {
	e, ticket, record := benchEngine(b)
	ctx := WithTicket(context.Background(), ticket)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.HandleUsage(ctx, record)
	}
	if err := e.Flush(context.Background()); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkAdmit measures the request-path cost with budgets configured, under parallel load.
func BenchmarkAdmit(b *testing.B) {
	e, _, _ := benchEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.Admit(keyX, "claude-fable-5(high)")
		}
	})
}

// BenchmarkAdmitNoConfig measures the request-path cost when no budget is configured.
func BenchmarkAdmitNoConfig(b *testing.B) {
	e := New()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.Admit(keyX, "claude-fable-5(high)")
		}
	})
}
