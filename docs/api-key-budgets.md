# API key token budgets

Cap how many tokens a client API key may use per day, week or month, per model family. Use it to keep
one consumer from draining a shared pool of upstream accounts, for example to cap Fable usage for an
agent fleet.

The feature is off by default. With nothing configured, requests, responses and usage accounting are
unchanged and the cost per request is a single pointer check.

## How it works

```
request ──> auth ──> admission ──> credential selection ──> upstream ──> usage record
                        │                  │                                  │
                  budget used up?    alias resolves to a                 charge the label's
                  429 + Retry-After  budgeted model? 429 (no cooldown)   counters (weighted)
```

1. **Admission.** After the client key is authenticated and before a credential is picked, the key is
   mapped to its label (`access.api-key-labels`). Every rule of that label that matches the requested
   model is looked at. If an **enforce** rule is used up (`used >= limit`) the request is answered with
   HTTP 429 and never reaches an upstream.
2. **Alias check.** A client may ask for an alias that resolves to a budgeted model. Admission only sees
   the alias, so the conductor checks again with the model the credential resolved. A rejection here
   makes no upstream attempt, records no failure and starts no cooldown, so the credential stays healthy.
3. **Charging.** When the request finishes, its usage record is charged to every matching rule of the
   label. Failed and cut-off requests are charged for the tokens already reported.

Counters are keyed by label, never by the raw key, so no secret appears in a counter, a log line or an
error.

## Configuration

Under `access:` (config v8; top-level `api-key-budgets:` in v7). Budgets are keyed by the labels of
`api-key-labels`:

```yaml
access:
  api-keys: ["k1", "k2"]
  api-key-labels:
    agents: "k1"
    monitor: "k2"
  api-key-budgets:
    mode: observe                 # observe (default) | enforce; a rule may override it
    timezone: UTC                 # IANA name; windows start at 00:00 there
    weights: {input: 1, output: 1, cache-write: 1, cache-read: 0.1}
    labels:
      agents:
        - {models: "claude-fable-*", window: week, limit: 2000000}
        - {models: "*", window: day, limit: 20000000, mode: enforce}
```

| Field | Meaning |
| --- | --- |
| `mode` | `observe` logs `budget would reject` once per rule and window and never blocks. `enforce` rejects. A rule's own `mode` wins. |
| `timezone` | Day, week (starting Monday) and month windows start at 00:00 in this zone. Windows are fixed calendar windows, correct across DST changes. |
| `weights` | Multiplier per token class. Unset weights default to 1, cache-read to 0.1. `0` makes a class free. |
| `models` | Wildcard pattern (`*`), case-insensitive. Matched against the model name without a thinking suffix, and against the name without its first `prefix/` segment. |
| `window` | `day`, `week` or `month`. |
| `limit` | Weighted tokens per window, greater than 0. |

Rules of one label are independent: **all** matching rules are checked and charged. A request is
rejected only when an enforce rule is used up. If several are, `Retry-After` is the latest reset among
them.

Validation fails the config (and a hot reload keeps the previous policy) for an unknown label, empty
`models`, unknown `window` or `mode`, a non-positive `limit`, a bad time zone, a negative weight or two
rules with the same `models` and `window`. Errors name labels only.

### Cost

The cost of a request is `uncached_input * input + output * output + cache_write * cache-write +
cache_read * cache-read` over the canonical token breakdown of the usage record. A Claude response with
100 input, 1000 cache-read and 50 output tokens costs `100 + 1000 * 0.1 + 50 = 250` with the defaults.
The overlapping `cached_tokens` counter is never used.

## What clients see

On a request that matches a rule, the rule with the **least remaining** is reported:

| Header | Value |
| --- | --- |
| `X-Budget-Remaining` | Weighted tokens left in that window (0 once used up). |
| `X-Budget-Reset` | When the window ends, RFC 3339 UTC. |
| `X-Budget-Mode` | `observe` or `enforce`. |

Keys without a label, models no rule matches and deployments without budgets get no `X-Budget-*`
header. The three headers are reserved, so an upstream cannot spoof them.

A rejected request gets HTTP 429, `Retry-After` in whole seconds, and an error body that names the label
and the rule, never the key:

```json
{"error":{"message":"token budget exhausted for label agents (models claude-fable-*, window week); resets at 2026-11-16T00:00:00Z","type":"rate_limit_error","code":"budget_exhausted"}}
```

Claude Messages endpoints use the Anthropic error shape (`{"type":"error","error":{"type":"rate_limit_error","message":...}}`),
which has no `code` field. Streams that are rejected before the first byte answer with the same 429, and
a rejected websocket turn is an error frame with `status: 429` and `headers.Retry-After`; the socket
stays open for the next turn.

## Roll out: observe first

1. Set `mode: observe` and your intended limits. Nothing is blocked. Watch the logs for
   `budget would reject (observe mode)` (fields `label`, `pattern`, `window`, `used`, `limit`) and read
   `X-Budget-Remaining` on responses.
2. Compare the limits with real use for at least one full window, then adjust.
3. Switch single rules to `mode: enforce`, or the global `mode`. The config reloads without a restart,
   and counters survive limit and mode changes.

Changing a rule's `models` or `window` starts a new counter, because those define the rule.

### Calibrating limits

- **Weekly cap for a scarce model** (for example Fable): `share * observed weekly capacity of the pool
  * 0.8`. The 0.8 leaves room for in-flight requests and for traffic that is not behind this proxy.
- **Daily cap for everything (`*`)**: about `1.5 * p95 of the label's daily use`, and not more than
  `weekly cap / 3`, so one bad day cannot spend the whole week.

## Persistence and reliability

- Counters live in the local KV store (`server.kv-store`, bbolt) and survive restarts. With the KV
  store disabled they are memory-only and reset on restart (logged once at start). They are per
  process: several proxy instances do not share counters.
- Usage is added in memory and written in batches every 2 seconds. A crash loses at most the last
  interval. Writing each record to bbolt measured about 105,750 ns against about 743 ns batched, which is
  why writes are batched.
- **Shutdown.** The usage manager stops first and delivers the records still queued, so they are charged;
  then the counters are flushed and the KV store closes. Both waits are capped at 5 seconds, so a stuck
  usage plugin or a stalled store cannot hold shutdown. If the flush has not finished by then, the store
  is left open for the process exit to reclaim.
- **The engine fails open.** KV errors, a corrupt counter, a panic or an invalid policy never reject a
  request; they are logged and the request runs. Only a used-up enforce rule rejects.
- No config means the policy is absent: admission is a nil check (about 35 ns, no allocations). The
  flusher does not run, the KV store is not read and nothing is logged.
- The flusher runs only while budgets are configured. Removing the config at a reload disables the engine
  and stops the flusher; adding budgets back starts it.
- Nothing blocks the request path or the usage dispatcher on I/O: admission and charging use in-memory
  counters. KV access happens in the background, in the flusher and in a one-off read of a counter's saved
  total the first time that counter is used. Until that read finishes (normally milliseconds) the counter
  holds only what this process has charged; the saved total is added when it arrives, so nothing is lost
  or counted twice. Usage is not written to the store before the read has succeeded, and a failed read is
  retried on the following flushes until it does.

## Limits

- **Overshoot.** A request is admitted while its rule has budget left and charged when it finishes
  (at stream end for streams). Concurrent in-flight requests can therefore take a rule past its limit
  by their own cost. The next request is rejected.
- **Codex streams.** A stream cancelled mid-way may report zero tokens and is then not charged.
- **Admission only.** Codex alpha search, Codex live calls (`/v1/realtime/calls`, `/v1/live/calls`) and
  the Muse Code relay publish no usage. They are rejected when the budget is used up but are not charged.
  The relay has no model at admission, so only rules with `models: "*"` apply to it.
- **Token counting.** `count_tokens` requests are neither charged nor rejected.
- **After a restart.** The first requests of a window can see more budget than is really left, until the
  saved total of their counters has loaded. The same window applies to a counter whose read keeps failing.
- **Websockets.** Every `response.create` turn is judged again, including follow-up frames on a duplex
  socket. Websocket responses carry no `X-Budget-*` headers.
- **Aliases.** The alias check runs once the conductor has selected a credential, on the models that
  credential resolves the requested name to. With a model pool alias, exhausted models are skipped and
  a model with budget left is used. If none is left, the request ends with the 429 instead of trying
  other credentials, so the first selected credential's resolved models decide: another credential that
  would resolve the alias to a model with budget left is not tried.
- **Plugin executors.** Routes served by a plugin executor are admitted on the requested model only; there
  is no upstream re-check, so an alias that resolves to a budgeted model is not rejected on them. Their
  usage is still charged to every rule that matches the model that served the request.
- **Home mode.** Budgets are not supported with Home enabled (the control plane routes and accounts
  requests). Configuring them there is a validation error.
- `/v0/management` endpoints are not part of this feature; config diffs in the log show label and
  rule counts only.
