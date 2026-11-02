# API key credential allowlist

Restrict a client API key to a chosen set of upstream credentials (accounts). Use it to keep a
low-priority consumer, such as a monitor, off the accounts your agents depend on.

The feature is off by default. With nothing configured, selection and request metadata are unchanged.

## Configuration

Both blocks live under `access:` (config v8):

```yaml
access:
  api-keys: ["k1", "k2"]                      # unchanged: client authentication
  api-key-labels:                             # label -> client key (secret-bearing: treat like api-keys)
    agents: "k1"
    monitor: "k2"
  api-key-auth-allowlist:
    monitor:
      auth-ids: ["claude-0123abcd.json", "codex-9f8e7d6c.json"]   # exact credential IDs
      tags: ["monitor-pool"]
```

- `auth-ids` match the exact credential ID (`Auth.ID`, for file credentials the file name).
- `tags` match credentials that carry the tag (see [Tags](#tags)).
- A credential is allowed when it matches **either** list (union).

## Semantics

| Situation | Result |
| --- | --- |
| Neither block configured | Behaviour is unchanged. |
| Key has no label | Full access. |
| Label has no entry in `api-key-auth-allowlist` | Full access. |
| Entry exists with `auth-ids` and/or `tags` | Only the union of both lists is eligible. |
| Entry exists but is empty (`monitor: {}` or `monitor:`) | Denies everything (fail closed). |
| Request without a resolved client key (no authentication, internal calls) | Not restricted. |

The allowlist only **narrows**. A credential must be in the allowlist **and** pass every existing
rule (provider and model support, disabled, cooldown, free-auth policy, already-tried set, session
affinity, priority). It can never make an otherwise ineligible credential eligible.

A request pinned to a credential outside the allowlist fails closed, with no fallback.

## Tags

A credential has tag `T` when `Attributes["tags"]` or `Metadata["tags"]` contains `T`. The value is a
comma separated string (`"a, monitor-pool"`) or a list of strings. Entries are trimmed, comparison is
case-sensitive, and empty entries or empty tags never match. `monitor-pool-extra` does not match
`monitor-pool`.

For file credentials, add `"tags": ["monitor-pool"]` to the auth JSON (it becomes `Metadata["tags"]`).
Credentials declared in `config.yaml` have no tags field; list them by `auth-ids`. `Attributes["tags"]`
is for credentials created by SDK embedders or plugins.

## Errors

When a restricted request has no eligible and available credential left (all allowed credentials are
cooling down, disabled, already tried, or the entry is empty), the client gets:

- HTTP `503`
- error `code` `api_key_allowlist_unavailable`
- message `no allowed account is currently available for this API key`

The response names no key, label or account. The server log gets a warning with the label, providers
and model (never the key). A request never falls through to a credential outside the allowlist, on
retry, failover, cooldown wait or session affinity.

If an allowed credential is tried and fails upstream, the client still sees that upstream error once
the allowed credentials are exhausted, exactly as without an allowlist. The `503` is returned when
nothing could be selected.

## Validation

Config load (and every reload) fails, with an error that names labels and never keys, when:

- a label is empty or has leading or trailing whitespace;
- a label's key is empty or not listed in `access.api-keys`;
- two labels name the same key;
- `api-key-auth-allowlist` has a label that is not in `api-key-labels`.

Duplicate YAML keys are rejected by the parser. An `auth-ids` entry that matches no loaded credential
is **not** an error: it logs a warning (`api-key-auth-allowlist "<label>" lists auth-ids that match no
loaded credential`) when the config is applied while credentials are loaded.

## Home mode

The allowlist cannot be enforced when Home picks credentials, so Home mode with any
`api-key-auth-allowlist` entry is a config error: the process refuses to start, and a Home config
that contains one is rejected. As defence in depth, a restricted request that reaches a Home pick
fails with the `503` above instead of becoming unrestricted.

## Redaction and hot reload

- `api-key-labels` holds client keys. The config change log prints only counts for it and for the
  allowlist, never keys, labels' keys or auth IDs.
- Config reload re-reads the allowlist for every new request. An invalid file is rejected and the
  last valid configuration (including its allowlist) stays in force.
- A reload publishes the labels and allowlists to the request handlers **before** it admits the client
  keys, so a newly added restricted key is never served under an older policy, even if the reload is
  cancelled half way. A request resolves its restriction from the configuration in force when it is
  dispatched (not when it authenticated): a key whose label or entry is removed by a reload is
  unrestricted from the next dispatch on, which is what the new configuration says.

## Limits

- Only requests whose client key is listed in `api-keys` can be labelled. Principals from other
  access providers have no label and keep full access.
- `/v1/models` is not filtered: it lists the models of all loaded credentials. A restricted key can
  see a model that none of its allowed credentials serves; the request then returns the `503`.
- When a model is served only by credentials outside the allowlist, a restricted key gets
  `503 api_key_allowlist_unavailable`. A model no credential serves keeps the usual model-not-found error.
- Plugin executors and model-router routes that do not use pool credentials are not affected.
- The management API (`/v0/management`) is unchanged and returns `api-keys` and `api-key-labels` as
  stored. Protect the management key accordingly.

## Example

```yaml
access:
  api-keys: ["agent-key", "monitor-key"]
  api-key-labels:
    agents: "agent-key"
    monitor: "monitor-key"
  api-key-auth-allowlist:
    monitor:
      tags: ["monitor-pool"]
```

Add `"tags": ["monitor-pool"]` to the auth JSON of the spare accounts. The `agents` key keeps using
every account; the `monitor` key can only use the tagged ones.

## Rollback

Remove the `api-key-auth-allowlist` and `api-key-labels` blocks (the config reloads without a
restart). Behaviour returns to full access for every key.
