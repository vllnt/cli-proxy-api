package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	claudehandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	budgetITLabel     = "team-x"
	budgetITKey       = "budget-it-key-team-x" // labelled
	budgetITOpenKey   = "budget-it-key-open"   // no label: budgets never apply
	budgetITSentinel  = "budget-it-sentinel"
	budgetITFable     = "claude-fable-5"
	budgetITSonnet    = "claude-sonnet-5"
	budgetITAlias     = "team-model" // resolves to budgetITFable on the credential
	budgetITUpstreamK = "sk-ant-budget-it-upstream"

	// One upstream response reports 100 uncached input, 1000 cache-read and 50 output tokens. With the
	// default weights (cache-read 0.1) that is 100 + 100 + 50 = 250 weighted tokens.
	budgetITRequestCost = 250
	// A stream cut right after message_start has only reported input and the first output token.
	budgetITPartialCost = 201
)

// budgetITNoon is a Saturday: the day window resets in 12h.
var budgetITNoon = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// budgetITUpstream is a Claude Messages upstream that reports cache_read_input_tokens.
type budgetITUpstream struct {
	server         *httptest.Server
	dropAfterStart atomic.Bool

	mu       sync.Mutex
	models   []string
	counts   atomic.Int32
	messages atomic.Int32
}

func newBudgetITUpstream(t *testing.T) *budgetITUpstream {
	t.Helper()
	u := &budgetITUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

func (u *budgetITUpstream) requestedModels() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

func (u *budgetITUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/count_tokens"):
		u.counts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":7}`)
	case strings.HasSuffix(r.URL.Path, "/v1/messages"):
		u.messages.Add(1)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		u.mu.Lock()
		u.models = append(u.models, req.Model)
		u.mu.Unlock()
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_budget","type":"message","role":"assistant","model":"`+req.Model+`","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000,"output_tokens":50}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		event := func(name, data string) {
			_, _ = io.WriteString(w, "event: "+name+"\ndata: "+data+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		event("message_start", `{"type":"message_start","message":{"id":"msg_budget","type":"message","role":"assistant","model":"`+req.Model+`","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000,"output_tokens":1}}}`)
		if u.dropAfterStart.Load() {
			panic(http.ErrAbortHandler) // the connection dies mid-stream
		}
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		event("content_block_stop", `{"type":"content_block_stop","index":0}`)
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}`)
		event("message_stop", `{"type":"message_stop"}`)
	default:
		http.NotFound(w, r)
	}
}

// budgetITTap observes usage records after the budget engine, which is registered before it: when the tap
// has a record, the engine has already charged it.
type budgetITTap struct {
	mu      sync.Mutex
	records []coreusage.Record
	wake    chan struct{}
}

func (p *budgetITTap) HandleUsage(_ context.Context, record coreusage.Record) {
	if !strings.HasPrefix(record.APIKey, "budget-it-") {
		return
	}
	p.mu.Lock()
	p.records = append(p.records, record)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// settle waits until want records of this test have been dispatched, then until everything published
// before this call has been (a sentinel record travels the same FIFO queue). It fails when the number of
// records differs from want, so "nothing was charged" is checked after the queue drained, not before.
func (p *budgetITTap) settle(t *testing.T, want int) []coreusage.Record {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		p.mu.Lock()
		have := len(p.records)
		p.mu.Unlock()
		if have >= want {
			break
		}
		select {
		case <-p.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %d usage records, have %d", want, have)
		}
	}
	const sentinelModel = "budget-it-sentinel-model"
	coreusage.PublishRecord(context.Background(), coreusage.Record{APIKey: budgetITSentinel, Provider: "budget-it", Model: sentinelModel})
	for {
		p.mu.Lock()
		for i, record := range p.records {
			if record.Model == sentinelModel {
				out := append([]coreusage.Record(nil), p.records[:i]...)
				p.records = p.records[i+1:]
				p.mu.Unlock()
				if len(out) != want {
					t.Fatalf("usage records = %d, want %d: %+v", len(out), want, out)
				}
				return out
			}
		}
		p.mu.Unlock()
		select {
		case <-p.wake:
		case <-deadline:
			t.Fatal("timed out waiting for the usage queue to drain")
		}
	}
}

type budgetITNoopPlugin struct{}

func (budgetITNoopPlugin) HandleUsage(context.Context, coreusage.Record) {}

type budgetITResultHook struct {
	coreauth.NoopHook
	results atomic.Int32
}

func (h *budgetITResultHook) OnResult(context.Context, coreauth.Result) { h.results.Add(1) }

func budgetITRule(models, window string, limit int64) sdkconfig.APIKeyBudgetRule {
	return sdkconfig.APIKeyBudgetRule{Models: models, Window: window, Limit: limit, Mode: "enforce"}
}

// installBudgetIT makes a fresh engine the process-wide one, charged from the default usage manager.
// Without rules no budget config exists at all.
func installBudgetIT(t *testing.T, rules ...sdkconfig.APIKeyBudgetRule) (*budget.Engine, *budgetITTap) {
	t.Helper()
	engine := budget.New(budget.WithClock(func() time.Time { return budgetITNoon }))
	if len(rules) > 0 {
		cfg := &sdkconfig.SDKConfig{
			APIKeys:      []string{budgetITKey, budgetITOpenKey},
			APIKeyLabels: map[string]string{budgetITLabel: budgetITKey},
			APIKeyBudgets: &sdkconfig.APIKeyBudgets{Labels: map[string][]sdkconfig.APIKeyBudgetRule{
				budgetITLabel: rules,
			}},
		}
		if err := engine.Apply(cfg); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
	}
	previous := budget.SetDefault(engine)
	tap := &budgetITTap{wake: make(chan struct{}, 1)}
	coreusage.RegisterNamedPlugin("budget-it-engine", engine)
	coreusage.RegisterNamedPlugin("budget-it-tap", tap)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin("budget-it-engine", budgetITNoopPlugin{})
		coreusage.RegisterNamedPlugin("budget-it-tap", budgetITNoopPlugin{})
		budget.SetDefault(previous)
	})
	return engine, tap
}

// remaining returns the tokens left for the labelled key on model, without charging anything.
func budgetITRemaining(t *testing.T, engine *budget.Engine, model string) int64 {
	t.Helper()
	decision, ticket := engine.Admit(budgetITKey, model)
	if ticket == nil || !decision.Applies {
		t.Fatalf("no budget rule applies to %q", model)
	}
	return decision.RemainingTokens
}

type budgetITFixture struct {
	t        *testing.T
	engine   *budget.Engine
	tap      *budgetITTap
	upstream *budgetITUpstream
	manager  *coreauth.Manager
	hook     *budgetITResultHook
	authID   string
	router   *gin.Engine
}

// newBudgetITFixture serves the Claude Messages routes in front of a real conductor and ClaudeExecutor
// and an httptest upstream. The request's client key is taken from the X-Test-Key header, as an access
// provider would set it.
func newBudgetITFixture(t *testing.T, rules ...sdkconfig.APIKeyBudgetRule) *budgetITFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine, tap := installBudgetIT(t, rules...)
	upstream := newBudgetITUpstream(t)

	cfg := &config.Config{}
	cfg.ClaudeKey = []config.ClaudeKey{{
		APIKey:  budgetITUpstreamK,
		BaseURL: upstream.server.URL,
		Models:  []config.ClaudeModel{{Name: budgetITFable, Alias: budgetITAlias}},
	}}
	hook := &budgetITResultHook{}
	manager := coreauth.NewManager(nil, nil, hook)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	authID := "budget-it-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	auth := &coreauth.Auth{
		ID:         authID,
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": budgetITUpstreamK, "base_url": upstream.server.URL},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: budgetITFable}, {ID: budgetITSonnet}, {ID: budgetITAlias}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	claude := claudehandlers.NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if key := c.GetHeader("X-Test-Key"); key != "" {
			c.Set("userApiKey", key)
		}
		c.Next()
	})
	router.POST("/v1/messages", claude.ClaudeMessages)
	router.POST("/v1/messages/count_tokens", claude.ClaudeCountTokens)
	return &budgetITFixture{t: t, engine: engine, tap: tap, upstream: upstream, manager: manager, hook: hook, authID: authID, router: router}
}

func (f *budgetITFixture) post(path, key, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-Test-Key", key)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func budgetITBody(model string, stream bool) string {
	return `{"model":"` + model + `","max_tokens":64,"stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hi"}]}`
}

func (f *budgetITFixture) message(key, model string, stream bool) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.post("/v1/messages", key, budgetITBody(model, stream))
}

func requireBudgetITStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
	}
}

func requireNoBudgetITHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for name := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(name), "x-budget-") {
			t.Fatalf("unexpected %s header: %v", name, rec.Header())
		}
	}
}

// requireBudgetITRejection checks the 429 a Claude Messages client sees: status, Retry-After, an Anthropic
// error body of type rate_limit_error naming the label and the pattern, and no leaked key. The Anthropic
// error schema has no code field, so budget_exhausted is only visible on OpenAI-format routes.
func requireBudgetITRejection(t *testing.T, rec *httptest.ResponseRecorder, pattern string) {
	t.Helper()
	requireBudgetITRejectionBody(t, rec, pattern, "")
}

func requireBudgetITRejectionBody(t *testing.T, rec *httptest.ResponseRecorder, pattern, wantCode string) {
	t.Helper()
	requireBudgetITStatus(t, rec, http.StatusTooManyRequests)
	if got := rec.Header().Get("Retry-After"); got != "43200" {
		t.Fatalf("Retry-After = %q, want 43200", got)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
	}
	if body.Error.Type != "rate_limit_error" || body.Error.Code != wantCode {
		t.Fatalf("error = %+v, want type rate_limit_error and code %q", body.Error, wantCode)
	}
	if !strings.Contains(body.Error.Message, budgetITLabel) || !strings.Contains(body.Error.Message, pattern) {
		t.Fatalf("message = %q, want the label %q and pattern %q", body.Error.Message, budgetITLabel, pattern)
	}
	for _, secret := range []string{budgetITKey, budgetITUpstreamK} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("the rejection leaks a key: %s", rec.Body.String())
		}
	}
}

func TestAPIKeyBudgetHeadersOnlyForBudgetedKeys(t *testing.T) {
	f := newBudgetITFixture(t, budgetITRule("claude-fable-*", "day", 1000))

	rec := f.message(budgetITKey, budgetITFable, false)
	requireBudgetITStatus(t, rec, http.StatusOK)
	for header, want := range map[string]string{
		"X-Budget-Remaining": "1000",
		"X-Budget-Reset":     "2026-11-15T00:00:00Z",
		"X-Budget-Mode":      "enforce",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	records := f.tap.settle(t, 1)
	if records[0].APIKey != budgetITKey || records[0].Failed {
		t.Fatalf("usage record = %+v", records[0])
	}
	if got := budgetITRemaining(t, f.engine, budgetITFable); got != 1000-budgetITRequestCost {
		t.Fatalf("remaining after one request = %d, want %d", got, 1000-budgetITRequestCost)
	}

	// A stream reports the budget too, with the usage of the first request already counted.
	rec = f.message(budgetITKey, budgetITFable, true)
	requireBudgetITStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("X-Budget-Remaining"); got != strconv.Itoa(1000-budgetITRequestCost) {
		t.Fatalf("stream X-Budget-Remaining = %q, want %d", got, 1000-budgetITRequestCost)
	}
	f.tap.settle(t, 1)

	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"a labelled key on a model no rule matches": func() *httptest.ResponseRecorder { return f.message(budgetITKey, budgetITSonnet, false) },
		"an unlabelled key":                         func() *httptest.ResponseRecorder { return f.message(budgetITOpenKey, budgetITFable, false) },
		"a request without a client key":            func() *httptest.ResponseRecorder { return f.message("", budgetITFable, false) },
	} {
		rec := call()
		requireBudgetITStatus(t, rec, http.StatusOK)
		requireNoBudgetITHeaders(t, rec)
		if t.Failed() {
			t.Fatalf("%s got budget headers", name)
		}
	}
	f.tap.settle(t, 2) // the keyless request is not a budget-it record
}

func TestAPIKeyBudgetNoConfigLeavesRequestsUnchanged(t *testing.T) {
	f := newBudgetITFixture(t)
	if f.engine.Enabled() {
		t.Fatal("an engine without config must be disabled")
	}
	for _, stream := range []bool{false, true} {
		rec := f.message(budgetITKey, budgetITFable, stream)
		requireBudgetITStatus(t, rec, http.StatusOK)
		requireNoBudgetITHeaders(t, rec)
	}
	f.tap.settle(t, 2)
	if got := f.upstream.messages.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

// capture is what a client observes of one response.
type budgetITCapture struct {
	status int
	header http.Header
	body   string
}

func captureBudgetIT(rec *httptest.ResponseRecorder) budgetITCapture {
	return budgetITCapture{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.String()}
}

func TestAPIKeyBudgetUnlabelledKeyIsUnaffected(t *testing.T) {
	observe := func(f *budgetITFixture) []budgetITCapture {
		var out []budgetITCapture
		for _, stream := range []bool{false, true} {
			out = append(out, captureBudgetIT(f.message(budgetITOpenKey, budgetITFable, stream)))
		}
		f.tap.settle(t, 2)
		return out
	}

	baseline := observe(newBudgetITFixture(t))

	f := newBudgetITFixture(t, budgetITRule("*", "day", 1))
	if rec := f.message(budgetITKey, budgetITFable, false); rec.Code != http.StatusOK {
		t.Fatalf("labelled request under budget: status %d", rec.Code)
	}
	f.tap.settle(t, 1)
	requireBudgetITRejection(t, f.message(budgetITKey, budgetITFable, false), "*")

	budgeted := observe(f)
	if !reflect.DeepEqual(baseline, budgeted) {
		t.Fatalf("an unlabelled key must see the exact responses it sees without any budget config:\nwithout budgets: %+v\nwith budgets:    %+v", baseline, budgeted)
	}
	for _, c := range budgeted {
		if c.status != http.StatusOK {
			t.Fatalf("unlabelled key status = %d: %s", c.status, c.body)
		}
	}
}

// Request 1 and 2 run under budget; the second one crosses the limit, so request 3 is rejected.
func TestAPIKeyBudgetRejectsOnceTheLimitIsCrossed(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			f := newBudgetITFixture(t, budgetITRule("claude-fable-*", "day", 400))

			requireBudgetITStatus(t, f.message(budgetITKey, budgetITFable, stream), http.StatusOK)
			f.tap.settle(t, 1)
			if got := budgetITRemaining(t, f.engine, budgetITFable); got != 400-budgetITRequestCost {
				t.Fatalf("remaining after one request = %d, want %d", got, 400-budgetITRequestCost)
			}
			// 150 tokens are left, so this request is admitted and overshoots the limit: it is never cut off.
			requireBudgetITStatus(t, f.message(budgetITKey, budgetITFable, stream), http.StatusOK)
			f.tap.settle(t, 1)
			if got := budgetITRemaining(t, f.engine, budgetITFable); got != 0 {
				t.Fatalf("remaining after crossing the limit = %d, want 0", got)
			}

			rec := f.message(budgetITKey, budgetITFable, stream)
			requireBudgetITRejection(t, rec, "claude-fable-*")
			if got := rec.Header().Get("X-Budget-Mode"); got != "enforce" {
				t.Fatalf("X-Budget-Mode on the rejection = %q, want enforce", got)
			}
			f.tap.settle(t, 0)
			if got := f.upstream.messages.Load(); got != 2 {
				t.Fatalf("upstream calls = %d, want 2: the rejected request must not reach an upstream", got)
			}
			if got := f.hook.results.Load(); got != 2 {
				t.Fatalf("conductor results = %d, want 2: a rejection is not an attempt", got)
			}

			// Other models, and other clients, are not affected by the exhausted rule.
			requireBudgetITStatus(t, f.message(budgetITKey, budgetITSonnet, stream), http.StatusOK)
			requireBudgetITStatus(t, f.message(budgetITOpenKey, budgetITFable, stream), http.StatusOK)
			f.tap.settle(t, 2)
		})
	}
}

func TestAPIKeyBudgetChargesAStreamThatFailsHalfWay(t *testing.T) {
	f := newBudgetITFixture(t, budgetITRule("claude-fable-*", "day", 1000))
	f.upstream.dropAfterStart.Store(true)

	rec := f.message(budgetITKey, budgetITFable, true)
	requireBudgetITStatus(t, rec, http.StatusOK) // the stream had started when it broke
	records := f.tap.settle(t, 1)
	if !records[0].Failed {
		t.Fatalf("the cut stream must be reported as failed: %+v", records[0])
	}
	if got := budgetITRemaining(t, f.engine, budgetITFable); got != 1000-budgetITPartialCost {
		t.Fatalf("remaining after a cut stream = %d, want %d: tokens the upstream already spent are charged", got, 1000-budgetITPartialCost)
	}

	// Failed requests count towards enforcement like any other.
	for i := 0; i < 4; i++ {
		requireBudgetITStatus(t, f.message(budgetITKey, budgetITFable, true), http.StatusOK)
		f.tap.settle(t, 1)
	}
	f.upstream.dropAfterStart.Store(false)
	requireBudgetITRejection(t, f.message(budgetITKey, budgetITFable, false), "claude-fable-*")
	f.tap.settle(t, 0)
}

func TestAPIKeyBudgetDoesNotChargeCountTokens(t *testing.T) {
	f := newBudgetITFixture(t, budgetITRule("claude-fable-*", "day", 1000))

	rec := f.post("/v1/messages/count_tokens", budgetITKey, `{"model":"`+budgetITFable+`","messages":[{"role":"user","content":"hi"}]}`)
	requireBudgetITStatus(t, rec, http.StatusOK)
	requireNoBudgetITHeaders(t, rec)
	if !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("body = %s, want a token count", rec.Body.String())
	}
	f.tap.settle(t, 0)
	if got := budgetITRemaining(t, f.engine, budgetITFable); got != 1000 {
		t.Fatalf("remaining after count_tokens = %d, want 1000", got)
	}

	// Counting tokens stays available once the budget is used up: it spends no generation tokens.
	requireBudgetITStatus(t, f.message(budgetITKey, budgetITFable, false), http.StatusOK)
	f.tap.settle(t, 1)
	for i := 0; i < 4; i++ {
		f.engine.HandleUsage(context.Background(), coreusage.Record{APIKey: budgetITKey, Provider: "claude", Model: budgetITFable, Detail: coreusage.Detail{InputTokens: 1000, TotalTokens: 1000}})
	}
	requireBudgetITRejection(t, f.message(budgetITKey, budgetITFable, false), "claude-fable-*")
	rec = f.post("/v1/messages/count_tokens", budgetITKey, `{"model":"`+budgetITFable+`","messages":[{"role":"user","content":"hi"}]}`)
	requireBudgetITStatus(t, rec, http.StatusOK)
}

// An alias is judged by the model it resolves to: the handler only sees the alias, the conductor re-checks.
func TestAPIKeyBudgetBlocksAnAliasOfABudgetedModel(t *testing.T) {
	f := newBudgetITFixture(t, budgetITRule("claude-fable-*", "day", 400))

	requireBudgetITStatus(t, f.message(budgetITKey, budgetITAlias, false), http.StatusOK)
	f.tap.settle(t, 1)
	if got := budgetITRemaining(t, f.engine, budgetITFable); got != 400-budgetITRequestCost {
		t.Fatalf("remaining = %d, want %d: an alias is charged to the model it ran on", got, 400-budgetITRequestCost)
	}
	requireBudgetITStatus(t, f.message(budgetITKey, budgetITAlias, true), http.StatusOK)
	f.tap.settle(t, 1)
	if got := f.upstream.requestedModels(); !reflect.DeepEqual(got, []string{budgetITFable, budgetITFable}) {
		t.Fatalf("upstream models = %v, want the alias resolved to %s", got, budgetITFable)
	}

	for _, stream := range []bool{false, true} {
		requireBudgetITRejection(t, f.message(budgetITKey, budgetITAlias, stream), "claude-fable-*")
	}
	f.tap.settle(t, 0)
	if got := f.upstream.messages.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2: an aliased request over budget must not reach the upstream", got)
	}
}

func TestAPIKeyBudgetAliasRejectionDoesNotCoolTheCredential(t *testing.T) {
	f := newBudgetITFixture(t, budgetITRule("claude-fable-*", "day", 200))

	requireBudgetITStatus(t, f.message(budgetITKey, budgetITFable, false), http.StatusOK)
	f.tap.settle(t, 1)
	resultsBefore := f.hook.results.Load()

	for _, stream := range []bool{false, true} {
		requireBudgetITRejection(t, f.message(budgetITKey, budgetITAlias, stream), "claude-fable-*")
	}
	if got := f.hook.results.Load(); got != resultsBefore {
		t.Fatalf("conductor results %d -> %d: an alias rejection must not record a result", resultsBefore, got)
	}
	auth, ok := f.manager.GetByID(f.authID)
	if !ok {
		t.Fatal("the credential disappeared")
	}
	if auth.Unavailable || auth.LastError != nil || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || auth.Status != coreauth.StatusActive {
		t.Fatalf("an alias rejection changed the credential: %+v", auth)
	}
	for model, state := range auth.ModelStates {
		if state.Unavailable || state.LastError != nil || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded {
			t.Fatalf("model %s is cooled down after a budget rejection: %+v", model, state)
		}
	}

	// The credential still serves everything that is not over budget.
	requireBudgetITStatus(t, f.message(budgetITKey, budgetITSonnet, false), http.StatusOK)
	requireBudgetITStatus(t, f.message(budgetITOpenKey, budgetITAlias, true), http.StatusOK)
	f.tap.settle(t, 2)
}

// budgetITPluginHost routes every model to one plugin executor that answers with a fixed OpenAI response.
type budgetITPluginHost struct {
	pluginID string
	calls    atomic.Int32
}

func (h *budgetITPluginHost) RouteModel(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: h.pluginID}, true
}

func (h *budgetITPluginHost) HasModelRouters() bool { return true }

func (h *budgetITPluginHost) ExecutePluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	h.calls.Add(1)
	return coreexecutor.Response{Payload: []byte(`{"id":"chatcmpl-budget","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":400,"completion_tokens":0,"total_tokens":400}}`)}, nil
}

func (h *budgetITPluginHost) ExecutePluginExecutorStream(context.Context, string, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	h.calls.Add(1)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl-budget","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":400,"completion_tokens":0,"total_tokens":400}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (h *budgetITPluginHost) CountPluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func TestAPIKeyBudgetChargesTheDirectPluginExecutorRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine, tap := installBudgetIT(t, budgetITRule("claude-fable-*", "day", 500))
	host := &budgetITPluginHost{pluginID: "budget-it-plugin"}
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	base.SetModelRouterHost(host)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userApiKey", c.GetHeader("X-Test-Key"))
		c.Next()
	})
	router.POST("/v1/chat/completions", openaihandlers.NewOpenAIAPIHandler(base).ChatCompletions)
	chat := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+budgetITFable+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Key", budgetITKey)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := chat()
	requireBudgetITStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("X-Budget-Remaining"); got != "500" {
		t.Fatalf("X-Budget-Remaining = %q, want 500", got)
	}
	tap.settle(t, 1)
	if got := budgetITRemaining(t, engine, budgetITFable); got != 100 {
		t.Fatalf("remaining after a plugin-executor request = %d, want 100: its usage must be charged", got)
	}

	requireBudgetITStatus(t, chat(), http.StatusOK) // admitted with 100 left; overshoot is charged
	tap.settle(t, 1)
	rec = chat()
	requireBudgetITRejectionBody(t, rec, "claude-fable-*", "budget_exhausted")
	tap.settle(t, 0)
	if got := host.calls.Load(); got != 2 {
		t.Fatalf("plugin executor calls = %d, want 2: a rejected request must not reach the plugin", got)
	}
}

// budgetITAccessProvider authenticates the labelled key as a Bearer token, like an access provider does.
type budgetITAccessProvider struct{}

func (budgetITAccessProvider) Identifier() string { return "budget-it-access" }

func (budgetITAccessProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if r == nil || r.Header.Get("Authorization") != "Bearer "+budgetITKey {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	return &sdkaccess.Result{Principal: budgetITKey, Provider: "budget-it-access"}, nil
}

func TestAPIKeyBudgetRejectsAFollowUpWebsocketTurn(t *testing.T) {
	f := newBudgetITFixture(t, budgetITRule("claude-fable-*", "day", 200))

	access := sdkaccess.NewManager()
	access.SetProviders([]sdkaccess.Provider{budgetITAccessProvider{}})
	responses := openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, f.manager))
	responses.AccessManager = access
	router := gin.New()
	router.GET("/v1/responses", responses.ResponsesWebsocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer " + budgetITKey}})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	turn := func() map[string]any {
		t.Helper()
		payload := `{"type":"response.create","model":"` + budgetITFable + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatalf("write turn: %v", err)
		}
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read turn: %v", err)
			}
			var event map[string]any
			if err := json.Unmarshal(msg, &event); err != nil {
				t.Fatalf("event is not JSON: %s", msg)
			}
			if kind, _ := event["type"].(string); kind == "response.completed" || kind == "error" {
				return event
			}
		}
	}

	if event := turn(); event["type"] != "response.completed" {
		t.Fatalf("turn 1 = %v, want response.completed", event)
	}
	f.tap.settle(t, 1) // the first turn used up the 200 token budget

	// The rejection answers the turn and keeps the socket open, so every later turn is judged again.
	for _, name := range []string{"turn 2", "turn 3"} {
		event := turn()
		if event["type"] != "error" {
			t.Fatalf("%s = %v, want an error frame", name, event)
		}
		if status, _ := event["status"].(float64); int(status) != http.StatusTooManyRequests {
			t.Fatalf("%s status = %v, want 429: %v", name, event["status"], event)
		}
		errBody, _ := event["error"].(map[string]any)
		if errBody["code"] != "budget_exhausted" {
			t.Fatalf("%s error = %v, want budget_exhausted", name, errBody)
		}
		headers, _ := event["headers"].(map[string]any)
		if headers["Retry-After"] != "43200" {
			t.Fatalf("%s headers = %v, want Retry-After 43200", name, headers)
		}
		if raw, _ := json.Marshal(event); strings.Contains(string(raw), budgetITKey) || strings.Contains(string(raw), budgetITUpstreamK) {
			t.Fatalf("%s leaks a key: %s", name, raw)
		}
	}
	if got := f.upstream.messages.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1: the rejected turn must not reach an upstream", got)
	}
}
