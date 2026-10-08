# Codex dispatch across subscription accounts

Status: proposal, 2026-10-08. Scope: the CLIPROXY01 deployment of this fork and its Codex OAuth
accounts (three `pro`, one `free`).

## What failed

- The upstream sheds load inside an HTTP 200 stream. It sends `response.created`,
  `response.in_progress`, heartbeats and sometimes an empty reasoning item, then an `error` event
  with code `server_is_overloaded`. The error arrived 11 s after the stream opened at the median,
  31 s at p90 and 43 s at most.
- `codex.stream-bootstrap-buffering` was off: that is the default, and production did not set it.
  So the proxy had already sent the handshake and could not fail over. Each of the 62 overloaded
  requests between 10:30 and 12:53 UTC was a single attempt and reached the client as a 200 stream.
  The Codex CLI shows that event as "Selected model is at capacity" and does not retry it.
- Each overload also cooled that (account, model) pair for the 60 s transient default.
- Load shedding did not depend on the account. Between 12:00 and 12:53 UTC it hit 9 %, 13 % and
  5 % of attempts on the three pro accounts. At about 10 % per request, a run of 10 requests fails
  65 % of the time; 17 of 29 runs (59 %) failed.

So the fix is to retry before the first byte reaches the client. Choosing a better account does
not help against overload. Dispatch matters for a separate problem, weekly quota: one account at
78 % weekly use carried 69 % of the traffic, while another sits at 8 %.

## Options

| Option | What | Stops overload from killing runs | Evens out weekly use | Cost | Maintenance |
|---|---|---|---|---|---|
| (a) Proxy-side | The existing bootstrap buffering, turned on in config, plus a short overload cooldown and a retry budget (this change). Quota-aware ranking (PR #1). Optional model fallback. | Yes | Yes, once PR #1's ranking is fixed | No per-token cost | Low: configuration and two small settings. PR #1 is about 2,000 lines, opt-in. |
| (b) Client-side | Paperclip retries provider-capacity failures without spending the run cap. | Partly: it restarts the turn and spends its tokens again | No | A Paperclip change | Low, but in another repository |
| (c) More capacity | More pro accounts, or a metered API-key tier as a last resort | More accounts: no, they are shed at the same rate. Metered tier: probably, not verified. | More accounts: yes | Per account or per token | Medium: accounts, keys and spend caps |

- With (a), if every account is shed the client gets a 503 before the stream starts. The Codex
  CLI retries such 5xx responses up to 4 times (`request_max_retries`). That is verified on
  openai/codex main, not on the CLI version the agents pin.
- A model fallback (for example astra to luna; the upstream advertises luna in
  `X-Codex-Safety-Buffering-Faster-Model`) silently swaps the model the client asked for. Also,
  gpt-6.1-sol was shed at the same time as gpt-6-astra. Not recommended by default.
- Only a metered tier adds capacity that is independent of the subscription. It needs a spend
  cap, and gpt-6-astra availability on the API platform is not verified.

## Recommendation

Choose (a), in this order:

1. Turn on failover before the first byte: this change plus the settings below. The settings
   allow up to three pro accounts across the first two retry rounds. At 5–13 % overload per
   attempt, about p³ ≈ 0.2 % of requests should still fail, and the CLI then retries those itself.
   This assumes attempts fail independently; that is not verified, and bursts would raise the
   rate.
2. Keep `routing.quota-aware` off until PR #1 ranks accounts by remaining quota per time to reset.
   As written, it sends every new session to the account whose window resets first, which is
   usually the most-used one. Once fixed, turn it on to even out weekly use.
3. Add (b) only if runs still fail after step 1. Add (c) only if measured capacity becomes the
   limit, rather than overload.

## Free-plan account (fl***)

It is already excluded from the premium models. Its catalog is compiled in, because the service
runs with `--local-model`, and `GetCodexFreeModels` contains neither gpt-6-astra nor
gpt-6.1-sol. That matches its zero premium traffic. Keep it that way: do not alias premium
models onto it.

It does share smaller models, such as gpt-5.6-terra and gpt-6-luna, with the pro accounts at the
same priority. Give its auth file a lower `priority`, so it serves those only when no pro account
is available.

## Production settings

```yaml
oauth:
  providers:
    codex:
      stream-bootstrap-buffering: true
      stream-bootstrap-timeout: "0"
routing:
  retry:
    request-retry: 1
    max-retry-credentials: 2
    max-retry-interval: 10
    max-retry-duration: 90
  cooldown:
    overload-cooldown-seconds: 5
```

Leave `stream-bootstrap-timeout` at "0". An overload that arrives after that ceiling is delivered
in the stream, and 10 % of overloads arrived after 31 s. The hold is still bounded by its 48-line
budget. With the observed two-line `: keep-alive` heartbeat every 4–5 s, that is about 23
heartbeats, or roughly 100 s. nginx's `proxy_read_timeout` is 1800 s.

`max-retry-duration` applies per execution. Production's `requests.streaming.bootstrap-retries: 1`
re-runs a failed streaming execution once. A client can therefore wait up to about twice the
budget, plus the duration of the last attempt already in flight, before it sees the 503.

The cost is time to first byte: response headers now wait for the first generated event, which
takes longer on reasoning turns. The client only loses the early handshake events, which carry no
output.
