// Package budget enforces per-client-API-key token budgets.
//
// Reliability contract: the engine only ever rejects a request on purpose, when an enforced
// rule is used up. Every internal failure (KV errors, panics, bad state, a policy that cannot
// compile) fails open: the request is admitted and the problem is logged. With no budget
// config the engine is a no-op: no allocation, locking, goroutine, KV access or logging.
package budget

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// DefaultFlushInterval is how often in-memory usage is written to the KV store.
// 0 writes after every usage record instead.
const DefaultFlushInterval = 2 * time.Second

const (
	// retention keeps a counter in the KV store this long after its window ends.
	retention = 7 * 24 * time.Hour
	// closeGrace keeps a closed window's counter in memory so streams that finish just after
	// the boundary are still charged to it.
	closeGrace = 5 * time.Minute
	// idleSweepInterval drives window-close detection and eviction when per-record flushing is on.
	idleSweepInterval = time.Minute
	loadTimeout       = 2 * time.Second
	flushTimeout      = 10 * time.Second
	warnEvery         = time.Minute
)

type counterKey struct {
	label  string
	ruleID string
	start  int64 // window start, unix seconds
}

// counter holds one rule's usage in one window. used is the running total (persisted base plus
// everything charged here); pending is the part not yet written to the KV store. The persisted base is
// added once, in the background (loading marks an attempt in flight, loaded that it succeeded), so
// until it arrives used holds only what this process charged.
type counter struct {
	label, pattern, window string
	kvKey                  string
	end                    time.Time

	loaded      atomic.Bool
	loading     atomic.Bool
	used        atomic.Int64
	pending     atomic.Int64
	limit       atomic.Int64
	observe     atomic.Bool
	warned      atomic.Bool
	wouldReject atomic.Int64
	written     atomic.Bool
	expiry      atomic.Bool
}

// Engine tracks budget usage and decides admission. The zero value is not usable; use New.
type Engine struct {
	pol       atomic.Pointer[policy]
	now       func() time.Time
	kv        func() (kvstore.KVClient, bool, error)
	interval  time.Duration
	newTicker func(time.Duration) (<-chan time.Time, func())
	// spawn runs a background load. Production starts a goroutine; tests can run it inline.
	spawn func(func())

	mu       sync.RWMutex
	counters map[counterKey]*counter

	// runMu guards the flusher lifecycle: runCtx is the context given to Start and stopRun stops the
	// running flusher (nil while none runs).
	runMu   sync.Mutex
	runCtx  context.Context
	stopRun context.CancelFunc

	noKVOnce sync.Once
	lastWarn atomic.Int64
}

// Option configures an Engine.
type Option func(*Engine)

// WithClock sets the time source (tests use a controllable clock).
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithKV sets how the engine finds its KV store.
func WithKV(get func() (kvstore.KVClient, bool, error)) Option {
	return func(e *Engine) {
		if get != nil {
			e.kv = get
		}
	}
}

// WithFlushInterval sets the KV write interval. 0 writes after every usage record.
func WithFlushInterval(d time.Duration) Option {
	return func(e *Engine) {
		if d >= 0 {
			e.interval = d
		}
	}
}

func withTicker(fn func(time.Duration) (<-chan time.Time, func())) Option {
	return func(e *Engine) { e.newTicker = fn }
}

// withInlineLoads runs counter loads on the calling goroutine so a test sees their effect at once.
func withInlineLoads() Option {
	return func(e *Engine) { e.spawn = func(f func()) { f() } }
}

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// New returns an engine with no policy. It admits everything until Apply installs a config.
func New(opts ...Option) *Engine {
	e := &Engine{
		now:       time.Now,
		kv:        kvstore.CurrentKVClient,
		interval:  DefaultFlushInterval,
		newTicker: realTicker,
		spawn:     func(f func()) { go f() },
		counters:  make(map[counterKey]*counter),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

var defaultEngine atomic.Pointer[Engine]

func init() { defaultEngine.Store(New()) }

// Default returns the process-wide engine used by the API handlers, executors and usage manager.
func Default() *Engine { return defaultEngine.Load() }

// SetDefault installs e as the process-wide engine and returns the engine it replaced. A nil e is
// ignored so Default never returns nil. It lets tests and embedders inject a clock or storage; call it
// before the server starts handling requests.
func SetDefault(e *Engine) *Engine {
	previous := Default()
	if e != nil {
		defaultEngine.Store(e)
	}
	return previous
}

// Apply compiles the config and installs it for all later requests. A nil config, or one without
// rules, disables budgets. An invalid config is reported and the previous policy stays active.
// Counters survive Apply: a rule keeps its count while its limit or mode changes, and a changed
// pattern or window is a new rule with a fresh counter. The flusher started by Start runs only while
// a policy is installed, so installing the first policy starts it and removing the last one stops it.
func (e *Engine) Apply(cfg *config.SDKConfig) error {
	pol, err := compilePolicy(cfg)
	if err != nil {
		return err
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.pol.Store(pol)
	e.syncFlusher()
	return nil
}

// Enabled reports whether any budget rule is active. Callers use it to skip all budget work, so a
// deployment without budgets pays one atomic load per request.
func (e *Engine) Enabled() bool { return e != nil && e.pol.Load() != nil }

// Decision is the outcome of Admit.
type Decision struct {
	// Applies reports whether any rule matched the requested model.
	Applies bool
	// Mode, RemainingTokens and ResetAt describe the matching rule with the least budget left.
	Mode            string
	RemainingTokens int64
	ResetAt         time.Time
	// Exhausted is non-nil when an enforced rule is used up and the request must be rejected.
	Exhausted *ExhaustedError
}

// Admit decides whether a request from apiKey for model may start. It returns the zero Decision and
// a nil Ticket for keys without a label or when budgets are not configured. A labelled key always
// gets a Ticket, even when no rule matches the requested model, so a later alias resolution to a
// budgeted upstream model can still be checked with CheckUpstream.
func (e *Engine) Admit(apiKey, model string) (dec Decision, ticket *Ticket) {
	defer func() {
		if r := recover(); r != nil {
			e.recovered("admit", r)
			dec, ticket = Decision{}, nil
		}
	}()
	pol := e.pol.Load()
	if pol == nil {
		return Decision{}, nil
	}
	label, ok := pol.labelByKey[strings.TrimSpace(apiKey)]
	if !ok {
		return Decision{}, nil
	}
	canonical := CanonicalModel(model)
	now := e.now()
	ticket = &Ticket{label: label}
	var (
		least      *rule
		leastLeft  int64
		leastEnd   time.Time
		blocker    *rule
		blockerEnd time.Time
	)
	for _, r := range pol.rules[label] {
		if !r.matches(canonical) {
			continue
		}
		start, end, ok := WindowBounds(r.window, pol.loc, now)
		if !ok {
			continue
		}
		c := e.counter(label, r, start, end)
		ticket.rules = append(ticket.rules, ticketRule{id: r.id, start: start, end: end})
		left := r.limitMilli - c.used.Load()
		if least == nil || left < leastLeft {
			least, leastLeft, leastEnd = r, left, end
		}
		if left > 0 {
			continue
		}
		if r.enforce {
			if blocker == nil || end.After(blockerEnd) {
				blocker, blockerEnd = r, end
			}
		} else {
			c.noteWouldReject()
		}
	}
	if least == nil {
		return Decision{}, ticket
	}
	dec = Decision{
		Applies:         true,
		Mode:            modeName(least),
		RemainingTokens: max(leastLeft, 0) / milliPerToken,
		ResetAt:         leastEnd,
	}
	if blocker != nil {
		dec.Exhausted = exhausted(label, blocker, blockerEnd, now)
	}
	return dec, ticket
}

// CheckUpstream re-checks a request against the rules that match upstreamModel but were not part of
// its admission ticket. The conductor calls it after alias resolution so a client cannot reach a
// budgeted model through an alias the rules do not name. It returns nil when the request may proceed.
func (e *Engine) CheckUpstream(t *Ticket, upstreamModel string) (result *ExhaustedError) {
	if e == nil || t == nil || t.label == "" {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			e.recovered("check-upstream", r)
			result = nil
		}
	}()
	pol := e.pol.Load()
	if pol == nil {
		return nil
	}
	canonical := CanonicalModel(upstreamModel)
	now := e.now()
	var (
		blocker    *rule
		blockerEnd time.Time
	)
	for _, r := range pol.rules[t.label] {
		if _, admitted := t.rule(r.id); admitted || !r.matches(canonical) {
			continue
		}
		start, end, ok := WindowBounds(r.window, pol.loc, now)
		if !ok {
			continue
		}
		c := e.counter(t.label, r, start, end)
		if r.limitMilli-c.used.Load() > 0 {
			continue
		}
		if r.enforce {
			if blocker == nil || end.After(blockerEnd) {
				blocker, blockerEnd = r, end
			}
		} else {
			c.noteWouldReject()
		}
	}
	if blocker == nil {
		return nil
	}
	return exhausted(t.label, blocker, blockerEnd, now)
}

// HandleUsage charges a usage record. It implements usage.Plugin. Each rule is charged at most once:
// the rules the request was admitted under (in the window it started in) plus any rule matching the
// model that actually served it. Without a ticket the label comes from the record's client key and
// the rules from its requested alias and served model. Failed and partial requests are charged.
func (e *Engine) HandleUsage(ctx context.Context, record usage.Record) {
	pol := e.pol.Load()
	if pol == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			e.recovered("charge", r)
		}
	}()
	ticket := TicketFromContext(ctx)
	var label string
	if ticket != nil {
		label = ticket.label
	} else {
		label = pol.labelByKey[strings.TrimSpace(record.APIKey)]
	}
	rules := pol.rules[label]
	if len(rules) == 0 {
		return
	}
	cost := CostMilli(record.Detail, record.Provider, record.ExecutorType, pol.weights)
	if cost <= 0 {
		return
	}
	served := CanonicalModel(record.Model)
	requested := ""
	if ticket == nil {
		requested = CanonicalModel(record.Alias)
	}
	var now time.Time
	for _, r := range rules {
		start, end, admitted := time.Time{}, time.Time{}, false
		if tr, ok := ticket.rule(r.id); ok {
			start, end, admitted = tr.start, tr.end, true
		}
		if !admitted && !r.matches(served) && (requested == "" || !r.matches(requested)) {
			continue
		}
		if !admitted {
			if now.IsZero() {
				now = e.now()
			}
			var ok bool
			if start, end, ok = WindowBounds(r.window, pol.loc, now); !ok {
				continue
			}
		}
		c := e.counter(label, r, start, end)
		satAdd(&c.used, cost)
		satAdd(&c.pending, cost)
		if e.interval == 0 {
			e.flushNow(c)
		}
	}
}

// counter returns the counter for a rule and window, creating it on first use. Creating one only
// schedules the read of its persisted total in the background; every call, first or not, is a map
// lookup and atomic stores, so no request ever waits for the store.
func (e *Engine) counter(label string, r *rule, start, end time.Time) *counter {
	key := counterKey{label: label, ruleID: r.id, start: start.Unix()}
	e.mu.RLock()
	c := e.counters[key]
	e.mu.RUnlock()
	created := false
	if c == nil {
		e.mu.Lock()
		if c = e.counters[key]; c == nil {
			c = &counter{
				label:   label,
				pattern: r.pattern,
				window:  r.window,
				end:     end,
				kvKey:   "budget:v1:" + kvstore.HashKeyPart(label) + ":" + r.id + ":" + strconv.FormatInt(key.start, 10),
			}
			e.counters[key] = c
			created = true
		}
		e.mu.Unlock()
	}
	c.limit.Store(r.limitMilli)
	c.observe.Store(!r.enforce)
	if created {
		e.spawn(func() { e.tryLoad(c) })
	}
	return c
}

// tryLoad adds the persisted total of c to its in-memory total, once. It runs off the request path.
// Usage charged meanwhile stays in used and in pending, which is not written until the load has
// succeeded, so the persisted total is neither double counted nor lost. A missing store or a failed read
// leaves c unloaded for a later flush to retry. At most one attempt per counter runs at a time. It never
// panics.
func (e *Engine) tryLoad(c *counter) {
	if c.loaded.Load() || !c.loading.CompareAndSwap(false, true) {
		return
	}
	defer c.loading.Store(false)
	defer func() {
		if r := recover(); r != nil {
			e.recovered("load", r)
		}
	}()
	client, ok, err := e.kvClient()
	if err != nil {
		e.warn("budget: kv store unavailable, a counter is not loaded yet", err)
		return
	}
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	raw, found, errGet := client.KVGet(ctx, c.kvKey)
	if errGet != nil {
		e.warn("budget: reading a counter failed, it is retried on the next flush", errGet)
		return
	}
	if found {
		if n, errParse := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64); errParse == nil && n > 0 {
			satAdd(&c.used, n)
		}
	}
	c.loaded.Store(true)
}

// retryLoad starts another load attempt for c when it is not loaded and none is running.
func (e *Engine) retryLoad(c *counter) {
	if !c.loaded.Load() && !c.loading.Load() {
		e.spawn(func() { e.tryLoad(c) })
	}
}

func (e *Engine) kvClient() (kvstore.KVClient, bool, error) {
	client, enabled, err := e.kv()
	if err != nil {
		return nil, false, err
	}
	if !enabled || client == nil {
		e.noKVOnce.Do(func() {
			log.Info("budget: no kv store is enabled; usage counters are memory-only and reset on restart")
		})
		return nil, false, nil
	}
	return client, true, nil
}

// Start lets the engine write pending usage to the KV store and close finished windows in the
// background until ctx is cancelled. The flusher runs only while a policy is installed: Start without
// budgets spawns nothing, and a later Apply with budgets starts it. Calling Start again replaces the
// previous run. The final flush belongs to the owner: call Flush before closing the KV store.
func (e *Engine) Start(ctx context.Context) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.haltFlusher()
	e.runCtx = ctx
	e.syncFlusher()
}

// syncFlusher starts or stops the flusher so that it runs exactly while the service is running and a
// policy is installed. The caller holds runMu.
func (e *Engine) syncFlusher() {
	want := e.runCtx != nil && e.runCtx.Err() == nil && e.pol.Load() != nil
	if !want {
		e.haltFlusher()
		return
	}
	if e.stopRun != nil {
		return
	}
	ctx, cancel := context.WithCancel(e.runCtx)
	e.stopRun = cancel
	interval := e.interval
	if interval <= 0 {
		interval = idleSweepInterval
	}
	tick, stop := e.newTicker(interval)
	go e.run(ctx, tick, stop)
}

func (e *Engine) haltFlusher() {
	if e.stopRun != nil {
		e.stopRun()
		e.stopRun = nil
	}
}

func (e *Engine) run(ctx context.Context, tick <-chan time.Time, stop func()) {
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if err := e.flush(ctx, false); err != nil {
				e.warn("budget: flush failed, usage is kept in memory and retried", err)
			}
		}
	}
}

// Flush writes all pending usage to the KV store, then logs and drops counters of closed windows.
// It is the final flush before the store closes, so it also writes counters whose persisted total has
// not loaded. Failed writes keep their usage pending for the next call. With no counters it returns at
// once without touching the KV store. It never panics.
func (e *Engine) Flush(ctx context.Context) error { return e.flush(ctx, true) }

// flush is Flush. The periodic flusher passes final=false, which holds back counters that are not
// loaded yet and retries their load: writing usage before the persisted total is known would count it
// twice once the total arrives.
func (e *Engine) flush(ctx context.Context, final bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e.recovered("flush", r)
			err = fmt.Errorf("budget: flush panic: %v", r)
		}
	}()
	e.mu.RLock()
	if len(e.counters) == 0 {
		e.mu.RUnlock()
		return nil
	}
	all := make([]*counter, 0, len(e.counters))
	for _, c := range e.counters {
		all = append(all, c)
	}
	e.mu.RUnlock()

	client, ok, kvErr := e.kvClient()
	if kvErr != nil {
		err = kvErr
	}
	if ok {
		now := e.now()
		for _, c := range all {
			if !final && !c.loaded.Load() {
				e.retryLoad(c)
				if !c.loaded.Load() {
					continue
				}
			}
			if flushErr := e.flushCounter(ctx, client, c, now); flushErr != nil && err == nil {
				err = flushErr
			}
		}
	}
	e.sweep(e.now(), ok)
	return err
}

// flushNow writes one counter immediately (flush interval 0).
func (e *Engine) flushNow(c *counter) {
	if !c.loaded.Load() {
		return
	}
	client, ok, err := e.kvClient()
	if err != nil {
		e.warn("budget: kv store unavailable, usage is kept in memory", err)
		return
	}
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := e.flushCounter(ctx, client, c, e.now()); err != nil {
		e.warn("budget: writing a counter failed, usage is kept in memory and retried", err)
	}
}

// flushCounter moves a counter's pending usage into the store. On any failure, including a panic in
// the store, the usage goes back to pending so nothing is lost or counted twice.
func (e *Engine) flushCounter(ctx context.Context, client kvstore.KVClient, c *counter, now time.Time) (err error) {
	delta := c.pending.Swap(0)
	stored := delta == 0
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("budget: kv panic: %v", r)
			e.recovered("kv", r)
		}
		if !stored {
			satAdd(&c.pending, delta)
		}
	}()
	if delta > 0 {
		if _, err = client.KVIncrBy(ctx, c.kvKey, delta); err != nil {
			return err
		}
		stored = true
		c.written.Store(true)
	}
	if c.written.Load() && !c.expiry.Load() {
		ttl := c.end.Sub(now) + retention
		updated, expErr := client.KVExpire(ctx, c.kvKey, ttl)
		if expErr != nil {
			return expErr
		}
		if updated {
			c.expiry.Store(true)
		}
	}
	return nil
}

// sweep drops counters whose window ended more than closeGrace ago and logs a summary for each.
// A counter that still has unwritten usage stays while the store is reachable, up to the retention.
func (e *Engine) sweep(now time.Time, storeUp bool) {
	var closed []*counter
	e.mu.Lock()
	for key, c := range e.counters {
		if now.Before(c.end.Add(closeGrace)) {
			continue
		}
		if storeUp && c.pending.Load() > 0 && now.Before(c.end.Add(retention)) {
			continue
		}
		delete(e.counters, key)
		closed = append(closed, c)
	}
	e.mu.Unlock()
	for _, c := range closed {
		fields := log.Fields{
			"label":        c.label,
			"pattern":      c.pattern,
			"window":       c.window,
			"used":         c.used.Load() / milliPerToken,
			"limit":        c.limit.Load() / milliPerToken,
			"would_reject": c.wouldReject.Load(),
		}
		if c.observe.Load() {
			log.WithFields(fields).Info("budget window closed (observe)")
		} else {
			log.WithFields(fields).Debug("budget window closed")
		}
	}
}

func (c *counter) noteWouldReject() {
	c.wouldReject.Add(1)
	if !c.warned.CompareAndSwap(false, true) {
		return
	}
	log.WithFields(log.Fields{
		"label":   c.label,
		"pattern": c.pattern,
		"window":  c.window,
		"used":    c.used.Load() / milliPerToken,
		"limit":   c.limit.Load() / milliPerToken,
	}).Warn("budget would reject (observe mode)")
}

func exhausted(label string, r *rule, end, now time.Time) *ExhaustedError {
	return &ExhaustedError{Label: label, Pattern: r.pattern, Window: r.window, ResetAt: end, RetryAfter: end.Sub(now)}
}

func modeName(r *rule) string {
	if r.enforce {
		return config.APIKeyBudgetModeEnforce
	}
	return config.APIKeyBudgetModeObserve
}

// warn logs at most one warning per minute so a failing store cannot flood the log. It uses the
// real clock on purpose: it is not part of window accounting and must work when the engine clock fails.
func (e *Engine) warn(msg string, err error) {
	nowNano := time.Now().UnixNano()
	last := e.lastWarn.Load()
	if nowNano-last < int64(warnEvery) || !e.lastWarn.CompareAndSwap(last, nowNano) {
		return
	}
	log.WithError(err).Warn(msg)
}

func (e *Engine) recovered(op string, r any) {
	log.WithField("op", op).Errorf("budget: recovered from panic, failing open: %v", r)
}

func satAdd(v *atomic.Int64, d int64) {
	for {
		cur := v.Load()
		next := cur + d
		if next < cur {
			next = math.MaxInt64
		}
		if v.CompareAndSwap(cur, next) {
			return
		}
	}
}
