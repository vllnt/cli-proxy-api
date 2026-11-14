package budget

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
)

const (
	keyX = "test-key-x"
	keyY = "test-key-y"
	keyZ = "test-key-z" // configured but unlabelled
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(value string) *fakeClock { return &fakeClock{t: utc(value)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = utc(value)
}

// fakeKV implements the three KV methods the engine uses; any other call panics on the nil embedded interface.
type fakeKV struct {
	kvstore.KVClient
	mu         sync.Mutex
	values     map[string]int64
	ttls       map[string]time.Duration
	getErr     error
	incrErr    error
	expireErr  error
	getPanic   bool
	incrPanic  bool
	incrCalls  int
	expireCall int
	getCalls   int
	// getGate, when set, makes KVGet report on getStarted and block until the gate is closed.
	getGate    chan struct{}
	getStarted chan struct{}
}

func newFakeKV() *fakeKV {
	return &fakeKV{values: map[string]int64{}, ttls: map[string]time.Duration{}}
}

func (f *fakeKV) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	f.getCalls++
	gate, started := f.getGate, f.getStarted
	f.mu.Unlock()
	if gate != nil {
		select {
		case started <- struct{}{}:
		default:
		}
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getPanic {
		panic("kv get exploded")
	}
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	v, ok := f.values[key]
	if !ok {
		return nil, false, nil
	}
	return []byte(fmt.Sprint(v)), true, nil
}

func (f *fakeKV) KVIncrBy(_ context.Context, key string, delta int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incrPanic {
		panic("kv incr exploded")
	}
	f.incrCalls++
	if f.incrErr != nil {
		return 0, f.incrErr
	}
	f.values[key] += delta
	return f.values[key], nil
}

func (f *fakeKV) KVExpire(_ context.Context, key string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireCall++
	if f.expireErr != nil {
		return false, f.expireErr
	}
	if _, ok := f.values[key]; !ok {
		return false, nil
	}
	f.ttls[key] = ttl
	return true, nil
}

func (f *fakeKV) total() (sum int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.values {
		sum += v
	}
	return sum
}

func staticKV(c kvstore.KVClient) Option {
	return WithKV(func() (kvstore.KVClient, bool, error) { return c, true, nil })
}

func sdkConfig(budgets *config.APIKeyBudgets) *config.SDKConfig {
	return &config.SDKConfig{
		APIKeys:       []string{keyX, keyY, keyZ},
		APIKeyLabels:  map[string]string{"team-x": keyX, "team-y": keyY},
		APIKeyBudgets: budgets,
	}
}

func rl(models, window string, limit int64, mode string) config.APIKeyBudgetRule {
	return config.APIKeyBudgetRule{Models: models, Window: window, Limit: limit, Mode: mode}
}

func budgetsFor(label string, rules ...config.APIKeyBudgetRule) *config.APIKeyBudgets {
	return &config.APIKeyBudgets{Labels: map[string][]config.APIKeyBudgetRule{label: rules}}
}

const saturdayNoon = "2026-11-14T12:00:00Z"

func newTestEngine(t *testing.T, budgets *config.APIKeyBudgets, opts ...Option) (*Engine, *fakeClock, *fakeKV) {
	t.Helper()
	clock := newFakeClock(saturdayNoon)
	kv := newFakeKV()
	base := []Option{WithClock(clock.Now), staticKV(kv), WithFlushInterval(time.Hour), withInlineLoads()}
	e := New(append(base, opts...)...)
	if err := e.Apply(sdkConfig(budgets)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	return e, clock, kv
}

func rec(apiKey, model string, outputTokens int64) usage.Record {
	return usage.Record{APIKey: apiKey, Model: model, Provider: "claude", Detail: detailFrom(0, 0, 0, outputTokens)}
}

// charge delivers a usage record as the usage manager would, carrying the admission ticket.
func charge(e *Engine, ticket *Ticket, record usage.Record) {
	e.HandleUsage(WithTicket(context.Background(), ticket), record)
}

func usedByPattern(e *Engine) map[string]int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string]int64{}
	for _, c := range e.counters {
		out[c.pattern+"|"+c.window] += c.used.Load()
	}
	return out
}

func captureLogs(t *testing.T) *logrustest.Hook {
	t.Helper()
	previous := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() { log.SetLevel(previous) })
	hook := logrustest.NewGlobal()
	hook.Reset()
	return hook
}

func countLogs(hook *logrustest.Hook, substr string) int {
	n := 0
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, substr) {
			n++
		}
	}
	return n
}

func TestAdmitUnderAtOver(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("claude-fable-*", "week", 100, "enforce")))

	d, ticket := e.Admit(keyX, "claude-fable-5")
	if !d.Applies || d.Exhausted != nil || d.RemainingTokens != 100 || d.Mode != "enforce" {
		t.Fatalf("fresh decision = %+v", d)
	}
	if ticket == nil || ticket.Label() != "team-x" {
		t.Fatalf("ticket = %+v", ticket)
	}
	if want := utc("2026-11-16T00:00:00Z"); !d.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want %s", d.ResetAt, want)
	}

	charge(e, ticket, rec(keyX, "claude-fable-5", 99))
	if d, _ = e.Admit(keyX, "claude-fable-5"); d.Exhausted != nil || d.RemainingTokens != 1 {
		t.Fatalf("under the limit must be admitted with 1 token left, got %+v", d)
	}

	charge(e, ticket, rec(keyX, "claude-fable-5", 1))
	d, _ = e.Admit(keyX, "claude-fable-5")
	if d.Exhausted == nil || d.RemainingTokens != 0 {
		t.Fatalf("at the limit must be rejected, got %+v", d)
	}
	if d.Exhausted.Label != "team-x" || d.Exhausted.Pattern != "claude-fable-*" || d.Exhausted.Window != "week" {
		t.Fatalf("error = %+v", d.Exhausted)
	}
	if d.Exhausted.RetryAfter != 36*time.Hour || d.Exhausted.SafeResponseHeaders().Get("Retry-After") != "129600" {
		t.Fatalf("RetryAfter = %v / %q, want 36h / 129600", d.Exhausted.RetryAfter, d.Exhausted.SafeResponseHeaders().Get("Retry-After"))
	}

	charge(e, ticket, rec(keyX, "claude-fable-5", 50))
	if d, _ = e.Admit(keyX, "claude-fable-5"); d.Exhausted == nil || d.RemainingTokens != 0 {
		t.Fatalf("over the limit must stay rejected with remaining floored at 0, got %+v", d)
	}
}

func TestAdmitIgnoresUnlabelledAndUnknownKeys(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 1, "enforce")))
	for _, key := range []string{keyZ, "test-key-nope", ""} {
		d, ticket := e.Admit(key, "claude-fable-5")
		if d != (Decision{}) || ticket != nil {
			t.Fatalf("key %q: decision = %+v ticket = %v, want none", key, d, ticket)
		}
		charge(e, nil, rec(key, "claude-fable-5", 1_000_000))
	}
	if got := usedByPattern(e); len(got) != 0 {
		t.Fatalf("unlabelled keys must never be counted, got %v", got)
	}
}

func TestAdmitWithoutPolicyIsFreeAndAllocationFree(t *testing.T) {
	e := New()
	if d, ticket := e.Admit(keyX, "claude-fable-5"); d != (Decision{}) || ticket != nil {
		t.Fatalf("no config must be a no-op, got %+v %v", d, ticket)
	}
	record := rec(keyX, "claude-fable-5", 10)
	ctx := context.Background()
	allocs := testing.AllocsPerRun(100, func() {
		e.Admit(keyX, "claude-fable-5")
		e.CheckUpstream(nil, "claude-fable-5")
		e.HandleUsage(ctx, record)
	})
	if allocs != 0 {
		t.Fatalf("no-config path allocates %v per call, want 0", allocs)
	}
	if err := e.Apply(nil); err != nil {
		t.Fatalf("Apply(nil) = %v", err)
	}
	if d, _ := e.Admit(keyX, "x"); d != (Decision{}) {
		t.Fatalf("Apply(nil) must disable, got %+v", d)
	}
}

func TestAdmitNonMatchingModelIssuesTicketWithoutApplying(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("claude-fable-*", "week", 1, "enforce")))
	d, ticket := e.Admit(keyX, "claude-sonnet-5")
	if d.Applies || d.Exhausted != nil || d.Mode != "" {
		t.Fatalf("decision = %+v, want not applicable", d)
	}
	if ticket == nil || ticket.Label() != "team-x" {
		t.Fatalf("a labelled key keeps a ticket so the conductor can check upstream models, got %v", ticket)
	}
}

func TestAdmitMatchesThinkingSuffixCaseAndPrefix(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("Claude-Fable-*", "day", 100, "enforce")))
	for _, model := range []string{"claude-fable-5(high)", "CLAUDE-FABLE-5", "antigravity/claude-fable-5(8192)"} {
		if d, _ := e.Admit(keyX, model); !d.Applies {
			t.Fatalf("model %q must match", model)
		}
	}
	if d, _ := e.Admit(keyX, "claude-sonnet-5(high)"); d.Applies {
		t.Fatal("claude-sonnet-5 must not match claude-fable-*")
	}
}

func TestAdmitReportsRuleWithLeastRemaining(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("claude-fable-*", "week", 500, "enforce"),
		rl("*", "day", 100, "observe"),
	))
	d, _ := e.Admit(keyX, "claude-fable-5")
	if d.RemainingTokens != 100 || d.Mode != "observe" || !d.ResetAt.Equal(utc("2026-11-15T00:00:00Z")) {
		t.Fatalf("headers must describe the day rule, got %+v", d)
	}
}

func TestAdmitRetryAfterUsesLatestResetOfExhaustedEnforceRules(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("*", "day", 10, "enforce"),
		rl("*", "week", 10, "enforce"),
	))
	_, ticket := e.Admit(keyX, "claude-fable-5")
	charge(e, ticket, rec(keyX, "claude-fable-5", 10))
	d, _ := e.Admit(keyX, "claude-fable-5")
	if d.Exhausted == nil || d.Exhausted.Window != "week" || d.Exhausted.RetryAfter != 36*time.Hour {
		t.Fatalf("both rules exhausted: retry must wait for the week reset, got %+v", d.Exhausted)
	}

	e2, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("*", "day", 10, "enforce"),
		rl("*", "week", 1000, "enforce"),
	))
	_, ticket = e2.Admit(keyX, "claude-fable-5")
	charge(e2, ticket, rec(keyX, "claude-fable-5", 10))
	d, _ = e2.Admit(keyX, "claude-fable-5")
	if d.Exhausted == nil || d.Exhausted.Window != "day" || d.Exhausted.RetryAfter != 12*time.Hour {
		t.Fatalf("only the day rule exhausted: retry must wait for midnight, got %+v", d.Exhausted)
	}
}

func TestAdmitHonorsConfiguredTimezone(t *testing.T) {
	budgets := budgetsFor("team-x", rl("*", "day", 100, "enforce"))
	budgets.Timezone = "Asia/Tokyo"
	e, _, _ := newTestEngine(t, budgets)
	d, _ := e.Admit(keyX, "m")
	if want := utc("2026-11-14T15:00:00Z"); !d.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want Tokyo midnight %s", d.ResetAt, want)
	}
}

func TestObserveModeNeverRejectsAndLogsOnce(t *testing.T) {
	hook := captureLogs(t)
	e, clock, _ := newTestEngine(t, budgetsFor("team-x", rl("claude-fable-*", "day", 10, "observe")))
	_, ticket := e.Admit(keyX, "claude-fable-5")
	charge(e, ticket, rec(keyX, "claude-fable-5", 15))
	for i := 0; i < 3; i++ {
		d, _ := e.Admit(keyX, "claude-fable-5")
		if d.Exhausted != nil || d.Mode != "observe" || d.RemainingTokens != 0 {
			t.Fatalf("observe must not reject, got %+v", d)
		}
	}
	if n := countLogs(hook, "budget would reject"); n != 1 {
		t.Fatalf("would-reject logged %d times, want once per counter per window", n)
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "budget would reject") {
			if entry.Data["label"] != "team-x" || entry.Data["pattern"] != "claude-fable-*" || entry.Data["window"] != "day" {
				t.Fatalf("log fields = %v", entry.Data)
			}
		}
		for _, v := range entry.Data {
			if s, ok := v.(string); ok && strings.Contains(s, keyX) {
				t.Fatalf("logs must never contain the key: %v", entry.Data)
			}
		}
		if strings.Contains(entry.Message, keyX) {
			t.Fatalf("logs must never contain the key: %q", entry.Message)
		}
	}

	hook.Reset()
	clock.Set("2026-11-15T00:06:00Z") // past the window end plus the late-charge grace
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	var summary *log.Entry
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "budget window closed") {
			summary = entry
		}
	}
	if summary == nil {
		t.Fatal("a window-close summary must be logged for an observe counter")
	}
	if summary.Data["used"] != int64(15) || summary.Data["limit"] != int64(10) || summary.Data["would_reject"] != int64(3) ||
		summary.Data["label"] != "team-x" || summary.Data["pattern"] != "claude-fable-*" || summary.Data["window"] != "day" {
		t.Fatalf("summary fields = %v", summary.Data)
	}
	if got := usedByPattern(e); len(got) != 0 {
		t.Fatalf("closed counters must be evicted, got %v", got)
	}
}

func TestObserveRuleDoesNotBlockWhenEnforceRuleHasRoom(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("*", "day", 5, "observe"),
		rl("*", "week", 1000, "enforce"),
	))
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 50))
	if d, _ := e.Admit(keyX, "m"); d.Exhausted != nil {
		t.Fatalf("an exhausted observe rule must not reject, got %+v", d.Exhausted)
	}
}

func TestGlobalModeAppliesWhenRuleHasNone(t *testing.T) {
	budgets := budgetsFor("team-x", rl("*", "day", 1, ""))
	budgets.Mode = "enforce"
	e, _, _ := newTestEngine(t, budgets)
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 1))
	if d, _ := e.Admit(keyX, "m"); d.Exhausted == nil {
		t.Fatal("global enforce must apply to a rule without its own mode")
	}
}

func TestAllMatchingRulesAreCharged(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("claude-fable-*", "week", 1000, "enforce"),
		rl("*", "day", 1000, "enforce"),
		rl("gpt-*", "day", 1000, "enforce"),
	))
	_, ticket := e.Admit(keyX, "claude-fable-5")
	charge(e, ticket, rec(keyX, "claude-fable-5", 7))
	got := usedByPattern(e)
	if got["claude-fable-*|week"] != 7_000 || got["*|day"] != 7_000 {
		t.Fatalf("both matching rules must be charged exactly once, got %v", got)
	}
	if _, ok := got["gpt-*|day"]; ok {
		t.Fatalf("a rule that does not match must not be touched, got %v", got)
	}
}

func TestChargeIncludesRulesMatchingTheUpstreamModel(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("my-*", "day", 1000, "enforce"),
		rl("claude-fable-*", "week", 1000, "enforce"),
	))
	d, ticket := e.Admit(keyX, "my-alias")
	if !d.Applies {
		t.Fatalf("alias must match the my-* rule, got %+v", d)
	}
	record := rec(keyX, "claude-fable-5", 11)
	record.Alias = "my-alias"
	charge(e, ticket, record)
	got := usedByPattern(e)
	if got["my-*|day"] != 11_000 || got["claude-fable-*|week"] != 11_000 {
		t.Fatalf("alias traffic must be charged to the upstream model's rule too, got %v", got)
	}
}

func TestChargeWithoutTicketUsesAliasAndModel(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("my-*", "day", 1000, "enforce"),
		rl("claude-fable-*", "week", 1000, "enforce"),
		rl("gpt-*", "day", 1000, "enforce"),
	))
	record := rec(keyX, "claude-fable-5", 3)
	record.Alias = "my-alias"
	e.HandleUsage(context.Background(), record)
	got := usedByPattern(e)
	if got["my-*|day"] != 3_000 || got["claude-fable-*|week"] != 3_000 {
		t.Fatalf("fallback must match alias and upstream model, got %v", got)
	}
	if _, ok := got["gpt-*|day"]; ok {
		t.Fatalf("unrelated rule charged: %v", got)
	}
}

func TestFailedRecordsAreChargedAndZeroCostIsIgnored(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 1000, "enforce")))
	_, ticket := e.Admit(keyX, "m")
	failed := rec(keyX, "m", 4)
	failed.Failed = true
	failed.Fail = usage.Failure{StatusCode: 502}
	charge(e, ticket, failed)
	if got := usedByPattern(e)["*|day"]; got != 4_000 {
		t.Fatalf("a failed request that consumed tokens must be charged, got %d", got)
	}
	charge(e, ticket, rec(keyX, "m", 0))
	if got := usedByPattern(e)["*|day"]; got != 4_000 {
		t.Fatalf("zero cost must be a no-op, got %d", got)
	}
}

func TestChargeUsesTheTicketWindowAcrossRollover(t *testing.T) {
	e, clock, _ := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 100, "enforce")))
	_, ticket := e.Admit(keyX, "m")
	clock.Set("2026-11-15T00:00:01Z")
	charge(e, ticket, rec(keyX, "m", 100))
	d, _ := e.Admit(keyX, "m")
	if d.Exhausted != nil || d.RemainingTokens != 100 {
		t.Fatalf("a stream that finishes after midnight belongs to the window it started in, got %+v", d)
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for key, c := range e.counters {
		if c.used.Load() == 100_000 && key.start != utc("2026-11-14T00:00:00Z").Unix() {
			t.Fatalf("charge landed in window %d", key.start)
		}
	}
}

func TestWindowRolloverStartsFresh(t *testing.T) {
	e, clock, _ := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 100, "enforce")))
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 100))
	if d, _ := e.Admit(keyX, "m"); d.Exhausted == nil {
		t.Fatal("must be exhausted before midnight")
	}
	clock.Set("2026-11-14T23:59:59Z")
	if d, _ := e.Admit(keyX, "m"); d.Exhausted == nil || d.Exhausted.RetryAfter != time.Second {
		t.Fatalf("one second before midnight, got %+v", d.Exhausted)
	}
	clock.Set("2026-11-15T00:00:00Z")
	if d, _ := e.Admit(keyX, "m"); d.Exhausted != nil || d.RemainingTokens != 100 {
		t.Fatalf("a new window must start empty, got %+v", d)
	}
}

func TestCheckUpstreamBlocksAliasToExhaustedModel(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("my-*", "day", 1000, "enforce"),
		rl("claude-fable-*", "week", 10, "enforce"),
	))
	_, ticket := e.Admit(keyX, "my-alias")
	if err := e.CheckUpstream(ticket, "claude-fable-5"); err != nil {
		t.Fatalf("under the limit must pass, got %v", err)
	}
	fable := rec(keyX, "claude-fable-5", 10)
	e.HandleUsage(context.Background(), fable)
	err := e.CheckUpstream(ticket, "claude-fable-5(high)")
	if err == nil || err.Pattern != "claude-fable-*" || err.Window != "week" || err.Label != "team-x" {
		t.Fatalf("alias to an exhausted model must be blocked, got %v", err)
	}
	if err := e.CheckUpstream(ticket, "claude-sonnet-5"); err != nil {
		t.Fatalf("a different upstream model must pass, got %v", err)
	}
	if err := e.CheckUpstream(nil, "claude-fable-5"); err != nil {
		t.Fatalf("nil ticket must pass, got %v", err)
	}
}

func TestCheckUpstreamSkipsRulesAlreadyAdmitted(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 10, "enforce")))
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 10))
	if err := e.CheckUpstream(ticket, "m"); err != nil {
		t.Fatalf("a rule admitted at the front door is not re-checked mid-flight, got %v", err)
	}
}

func TestCheckUpstreamObserveDoesNotBlock(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x",
		rl("my-*", "day", 1000, "enforce"),
		rl("claude-fable-*", "week", 1, "observe"),
	))
	_, ticket := e.Admit(keyX, "my-alias")
	e.HandleUsage(context.Background(), rec(keyX, "claude-fable-5", 5))
	if err := e.CheckUpstream(ticket, "claude-fable-5"); err != nil {
		t.Fatalf("observe rules never block, got %v", err)
	}
}

func TestHotReload(t *testing.T) {
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("claude-fable-*", "week", 100, "enforce")))
	_, ticket := e.Admit(keyX, "claude-fable-5")
	charge(e, ticket, rec(keyX, "claude-fable-5", 50))

	if err := e.Apply(sdkConfig(budgetsFor("team-x", rl("claude-fable-*", "week", 60, "enforce")))); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.Admit(keyX, "claude-fable-5"); d.RemainingTokens != 10 {
		t.Fatalf("a limit change must keep the count: remaining %d, want 10", d.RemainingTokens)
	}

	if err := e.Apply(sdkConfig(budgetsFor("team-x", rl("claude-*", "week", 60, "enforce")))); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.Admit(keyX, "claude-fable-5"); d.RemainingTokens != 60 {
		t.Fatalf("a pattern change is a new rule with a fresh counter: remaining %d, want 60", d.RemainingTokens)
	}

	// A ticket issued before the reload may name a rule that no longer exists: it is ignored.
	_, oldTicket := e.Admit(keyX, "claude-fable-5")
	if err := e.Apply(sdkConfig(budgetsFor("team-x", rl("gpt-*", "day", 100, "enforce")))); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.Admit(keyX, "claude-fable-5"); d.Applies {
		t.Fatalf("a removed rule must not apply, got %+v", d)
	}
	before := usedByPattern(e)["claude-*|week"]
	charge(e, oldTicket, rec(keyX, "claude-fable-5", 9))
	if after := usedByPattern(e)["claude-*|week"]; after != before {
		t.Fatalf("a removed rule must not be charged: %d -> %d", before, after)
	}
	_ = kv

	if err := e.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if d, tk := e.Admit(keyX, "claude-fable-5"); d != (Decision{}) || tk != nil {
		t.Fatalf("removing the config disables the engine, got %+v %v", d, tk)
	}
}

func TestApplyInvalidConfigKeepsLastGoodPolicy(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 100, "enforce")))
	bad := sdkConfig(budgetsFor("team-x", rl("*", "hour", 100, "enforce")))
	if err := e.Apply(bad); err == nil {
		t.Fatal("an invalid config must be reported")
	}
	if d, _ := e.Admit(keyX, "m"); !d.Applies || d.RemainingTokens != 100 {
		t.Fatalf("the last good policy must stay active, got %+v", d)
	}
}

func TestFlushPersistsDeltasWithPrivacyAndTTL(t *testing.T) {
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("claude-fable-*", "week", 100, "enforce")))
	_, ticket := e.Admit(keyX, "claude-fable-5")
	charge(e, ticket, rec(keyX, "claude-fable-5", 40))
	if kv.incrCalls != 0 {
		t.Fatal("charging must not touch the KV store on the hot path")
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	if len(kv.values) != 1 || kv.total() != 40_000 {
		t.Fatalf("persisted = %v", kv.values)
	}
	for key := range kv.values {
		wantPrefix := "budget:v1:" + kvstore.HashKeyPart("team-x") + ":" + ruleID("claude-fable-*", "week") + ":" +
			fmt.Sprint(utc("2026-11-09T00:00:00Z").Unix())
		if key != wantPrefix {
			t.Fatalf("kv key = %q, want %q", key, wantPrefix)
		}
		if strings.Contains(key, keyX) || strings.Contains(key, "team-x") {
			t.Fatalf("kv key leaks the key or label: %q", key)
		}
		// remaining window (36h) + 7 days of retention
		if want := 36*time.Hour + 7*24*time.Hour; kv.ttls[key] != want {
			t.Fatalf("ttl = %v, want %v", kv.ttls[key], want)
		}
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if kv.incrCalls != 1 || kv.expireCall != 1 {
		t.Fatalf("an idle flush must not write again: incr=%d expire=%d", kv.incrCalls, kv.expireCall)
	}
	charge(e, ticket, rec(keyX, "claude-fable-5", 2))
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if kv.total() != 42_000 || kv.expireCall != 1 {
		t.Fatalf("second flush adds only the delta and keeps the ttl: total=%d expire=%d", kv.total(), kv.expireCall)
	}
}

func TestFlushErrorRetainsPendingUsage(t *testing.T) {
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 100, "enforce")))
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 10))

	kv.incrErr = errors.New("disk on fire")
	if err := e.Flush(context.Background()); err == nil {
		t.Fatal("Flush must report the KV error")
	}
	if kv.total() != 0 {
		t.Fatal("nothing may be persisted while the store fails")
	}
	if d, _ := e.Admit(keyX, "m"); d.RemainingTokens != 90 {
		t.Fatalf("in-memory usage must survive a failed flush, remaining %d", d.RemainingTokens)
	}

	kv.incrErr = nil
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() after recovery = %v", err)
	}
	if kv.total() != 10_000 {
		t.Fatalf("retained usage must be persisted exactly once, total = %d", kv.total())
	}
}

func TestKVGetErrorFailsOpenAndStillCounts(t *testing.T) {
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 10, "enforce")))
	kv.getErr = errors.New("boom")
	d, ticket := e.Admit(keyX, "m")
	if d.Exhausted != nil || d.RemainingTokens != 10 {
		t.Fatalf("a KV read failure must not reject, got %+v", d)
	}
	charge(e, ticket, rec(keyX, "m", 10))
	if d, _ = e.Admit(keyX, "m"); d.Exhausted == nil {
		t.Fatal("in-memory counting continues, so exhaustion is still detected")
	}
}

func TestKVPanicFailsOpen(t *testing.T) {
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 10, "enforce")))
	kv.getPanic = true
	d, ticket := e.Admit(keyX, "m")
	if d.Exhausted != nil {
		t.Fatalf("a panicking store must not reject, got %+v", d)
	}
	charge(e, ticket, rec(keyX, "m", 1))
	kv.getPanic = false
	kv.incrPanic = true
	if err := e.Flush(context.Background()); err == nil {
		t.Fatal("a panic during flush must surface as an error, not crash")
	}
}

func TestEnginePanicFailsOpen(t *testing.T) {
	e, _, _ := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 10, "enforce")))
	e.now = func() time.Time { panic("clock exploded") }
	d, ticket := e.Admit(keyX, "m")
	if d != (Decision{}) {
		t.Fatalf("a panic in Admit must allow the request, got %+v", d)
	}
	e.HandleUsage(WithTicket(context.Background(), ticket), rec(keyX, "m", 1))
	if err := e.CheckUpstream(&Ticket{label: "team-x"}, "m"); err != nil {
		t.Fatalf("a panic in CheckUpstream must allow the request, got %v", err)
	}
}

func TestRestartKeepsCountersWithBoltStore(t *testing.T) {
	store, err := kvstore.OpenBoltStore(filepath.Join(t.TempDir(), "kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clock := newFakeClock(saturdayNoon)
	cfg := sdkConfig(budgetsFor("team-x", rl("claude-fable-*", "week", 100, "enforce")))
	newEngine := func() *Engine {
		e := New(WithClock(clock.Now), staticKV(store), WithFlushInterval(time.Hour), withInlineLoads())
		if applyErr := e.Apply(cfg); applyErr != nil {
			t.Fatal(applyErr)
		}
		return e
	}

	first := newEngine()
	_, ticket := first.Admit(keyX, "claude-fable-5")
	charge(first, ticket, rec(keyX, "claude-fable-5", 70))
	if err := first.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	second := newEngine()
	d, _ := second.Admit(keyX, "claude-fable-5")
	if d.RemainingTokens != 30 {
		t.Fatalf("after restart remaining = %d, want 30", d.RemainingTokens)
	}
	_, ticket = second.Admit(keyX, "claude-fable-5")
	charge(second, ticket, rec(keyX, "claude-fable-5", 30))
	if err := second.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	third := newEngine()
	if d, _ = third.Admit(keyX, "claude-fable-5"); d.Exhausted == nil {
		t.Fatalf("the persisted total must reach the limit across restarts, got %+v", d)
	}

	clock.Set("2026-11-16T00:00:00Z") // next week
	if d, _ = third.Admit(keyX, "claude-fable-5"); d.Exhausted != nil || d.RemainingTokens != 100 {
		t.Fatalf("next week starts empty, got %+v", d)
	}
}

func TestMemoryOnlyWhenNoKVStoreIsLoggedOnce(t *testing.T) {
	hook := captureLogs(t)
	clock := newFakeClock(saturdayNoon)
	e := New(WithClock(clock.Now), WithKV(func() (kvstore.KVClient, bool, error) { return nil, false, nil }), WithFlushInterval(time.Hour))
	if err := e.Apply(sdkConfig(budgetsFor("team-x", rl("*", "day", 10, "enforce"), rl("*", "week", 10, "enforce")))); err != nil {
		t.Fatal(err)
	}
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 10))
	if d, _ := e.Admit(keyX, "m"); d.Exhausted == nil {
		t.Fatal("memory-only counting must still enforce")
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() without a store = %v", err)
	}
	n := 0
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "memory-only") {
			n++
			if entry.Level != log.InfoLevel {
				t.Fatalf("memory-only notice level = %v, want info", entry.Level)
			}
		}
	}
	if n != 1 {
		t.Fatalf("memory-only notice logged %d times, want once", n)
	}
}

func TestPerRecordFlushWithZeroInterval(t *testing.T) {
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 100, "enforce")), WithFlushInterval(0))
	_, ticket := e.Admit(keyX, "m")
	charge(e, ticket, rec(keyX, "m", 6))
	if kv.total() != 6_000 {
		t.Fatalf("interval 0 persists every record immediately, total = %d", kv.total())
	}
	charge(e, ticket, rec(keyX, "m", 4))
	if kv.total() != 10_000 {
		t.Fatalf("total = %d, want 10000", kv.total())
	}
}

func TestConcurrentUseIsRaceFreeAndLosesNothing(t *testing.T) {
	e, _, kv := newTestEngine(t, budgetsFor("team-x", rl("*", "day", 1_000_000, "enforce"), rl("claude-*", "week", 1_000_000, "enforce")))
	const workers, perWorker = 8, 200
	cfg := sdkConfig(budgetsFor("team-x", rl("*", "day", 1_000_000, "enforce"), rl("claude-*", "week", 1_000_000, "enforce")))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_, ticket := e.Admit(keyX, "claude-fable-5")
				charge(e, ticket, rec(keyX, "claude-fable-5", 1))
			}
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = e.Apply(cfg)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = e.Flush(context.Background())
		}
	}()
	wg.Wait()
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// two rules match, so each charge lands twice
	if want := int64(workers*perWorker) * 1000 * 2; kv.total() != want {
		t.Fatalf("persisted total = %d, want %d", kv.total(), want)
	}
}

func TestEnabledFollowsPolicy(t *testing.T) {
	e := New()
	if e.Enabled() {
		t.Fatal("a new engine has no policy")
	}
	if err := e.Apply(sdkConfig(budgetsFor("team-x", rl("*", "day", 1, "enforce")))); err != nil {
		t.Fatal(err)
	}
	if !e.Enabled() {
		t.Fatal("Apply with rules must enable the engine")
	}
	if err := e.Apply(sdkConfig(&config.APIKeyBudgets{Mode: "enforce"})); err != nil {
		t.Fatal(err)
	}
	if e.Enabled() {
		t.Fatal("a config without rules must disable the engine")
	}
}

func TestSetDefaultSwapsTheProcessEngine(t *testing.T) {
	original := Default()
	replacement := New()
	if previous := SetDefault(replacement); previous != original {
		t.Fatal("SetDefault must return the engine it replaced")
	}
	t.Cleanup(func() { SetDefault(original) })
	if Default() != replacement {
		t.Fatal("Default must return the installed engine")
	}
	if SetDefault(nil); Default() != replacement {
		t.Fatal("SetDefault(nil) must be ignored so Default never returns nil")
	}
}
