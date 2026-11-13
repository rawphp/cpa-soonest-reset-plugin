# soonest-reset: a CLIProxyAPI scheduler plugin

A plugin for the CLIProxyAPI gateway. It sends each Claude request to the account whose weekly quota resets soonest, and it stops stale cooldown records from blocking accounts that have quota.

Target gateway: CLIProxyAPI v8.0.51, darwin/arm64. Work from the CLIProxyAPI v8.0.51 source. Read it, and never run code from it except through `go build`.

## The two problems

1. **Wrong account order.** The gateway uses fill-first over a hand-set `priority`. Weekly quota left unused at a reset is lost, so the account closest to its weekly reset should be spent first.
2. **Stale cooldowns.** On a 429 the gateway records a cooldown until the reset time in the response headers, per account and model, and never tests the account again before that time. The gateway also restores these records after a restart. When the provider returns quota early, the record goes stale and blocks an account that has quota, sometimes for days. When every account is blocked that way, requests fail with `503 auth_unavailable`.

## What the plugin does

It declares two capabilities, `scheduler` and `usage_plugin`, and uses three host callbacks: `host.auth.list`, `host.routing.reset_cooldown` and `host.log`.

### State, in memory, per account

- `reset7d`: the weekly reset time from the last response headers, or none.
- `observed`: a response has been seen for this account since the plugin started or since its last retest.
- `pending`: picked as a probe at a recorded time and not answered yet. It lapses after a fixed two minutes.
- `probeDue`: the next pick should try this account once.
- `rateLimited`: the last response for this account was a 429.

Nothing is written to disk. After a gateway restart every account is unobserved and is probed once.

### On every usage record (`usage.handle`)

For provider `claude` only. Read `Anthropic-Ratelimit-Unified-7d-Reset` from the response headers (a Unix timestamp, or an RFC 3339 time). Set `observed`, clear `pending`, and set `rateLimited` when the record failed with status 429. Failed records carry the response headers too. When the header is present, store its value as `reset7d` if it parses and is in the future, and clear `reset7d` otherwise, so a stored past reset does not keep the account due. When the header is absent, keep `reset7d` as it is. A failure with no rate-limit headers (a 500, a 529, a 401, a dropped stream) says nothing about the weekly window. A record with no such header still sets `observed`, so an account that never sends the header is not probed forever.

### On every pick (`scheduler.pick`)

Declare `SchedulerAcrossPriorities` so the candidates include every available account, not only the top priority tier.

1. Keep the candidates whose provider is `claude`. If there are none, answer unhandled and the built-in scheduler picks.
2. If any candidate is unobserved, or `probeDue`, or has a `reset7d` in the past: pick the one with the highest `priority` (then lowest ID), clear its `probeDue`, and mark it `pending` so a second request does not probe it again before the first answers. A `pending` mark older than two minutes no longer blocks a probe, because some requests go through account selection and never produce a usage record.
3. Otherwise pick the candidate with the earliest `reset7d`. Ties go to the higher `priority`, then the lower ID. A candidate with no `reset7d` sorts after every candidate that has one.
A token count, marked by `request_path` `/v1/messages/count_tokens` in `Options.Metadata`, goes through account selection but the gateway publishes no usage record for it. It is picked by rule 3 alone and never spends a probe.

4. Log each pick with `host.log`: the account, the rule that chose it and the reset time. The gateway's log formatter prints only fields it knows, so these go in the message text.

The plugin never rejects a pick and never returns an error for bad input. On any internal failure it answers unhandled, so the gateway falls back to fill-first.

Exhausted accounts need no tracking in the plugin. The gateway cools them and leaves them out of the candidates.

### Retest, at startup and then once per `retest_minutes` (default 60)

For every `claude` account from `host.auth.list`, call `host.routing.reset_cooldown`. Then, for each account whose last response was a 429, set `probeDue`, clear `observed` and clear `rateLimited`. The next request tries it. An account that is still exhausted answers 429, the gateway cools it again with a current reset time, and the same request moves to the next account. The cost is one failed attempt per cooled account per interval.

The reset response's model list does not say whether the account had a cooldown. The gateway returns every model-state key, or every registered model when there are none, so the list is almost never empty. The plugin's own record of the last answer is the signal instead. An account the plugin has not seen answer is probed anyway.

The retest runs from a ticker goroutine that checks once a minute. It starts at `plugin.register`, stops at `plugin.quiesce` and shutdown, and restarts on `plugin.reconfigure`. It does not run inside `scheduler.pick`: the host only calls the scheduler when at least one account is available, so when every account is cooled no pick happens and a pick-driven retest would never run. Host callbacks are identified by the plugin's host context and need no `host_callback_id`.

The first pass runs at the ticker's first tick, a minute after `plugin.register`, so a cooldown the gateway restored at startup does not block an account for a full interval. It does not run inside `plugin.register` itself, because the gateway registers plugins before it binds the auth manager, so the callbacks would find no accounts.

`host.auth.list` returns only file-backed accounts. Claude API keys from the gateway's config are not in it, so the retest does not reach them. They are still ordered by weekly reset.

### Config

```yaml
plugins:
  enabled: true
  configs:
    soonest-reset:
      enabled: true
      priority: 1
      retest_minutes: 60   # 0 turns the retest off
      log_picks: true
```

`plugin.reconfigure` applies a new config without losing state.

## Known trade-off: session affinity

When a plugin handles a pick, the gateway skips its session-affinity selector (`sdk/cliproxy/auth/conductor_selection.go`, the `if !handled` branch). The order here is stable, so a session stays on one account until that account is exhausted or a reset passes. A probe moves one request to another account, which costs one prompt-cache miss. This version does not keep a session map. The pick log is there so the cache-read rate can be compared before and after.

## Layout

```
go.mod                      module, requires github.com/router-for-me/CLIProxyAPI/v8 v8.0.51
main.go                     cgo glue only: init, call, free, shutdown, host call. No logic.
internal/quota/             header parsing -> weekly reset time
internal/state/             the per-account store, safe for concurrent use
internal/pick/              the pick rule as a pure function
internal/retest/            the retest pass, against a Host interface
internal/plugin/            method dispatch: register, reconfigure, scheduler.pick, usage.handle
scripts/build.sh            builds plugins/darwin/arm64/soonest-reset.dylib
scripts/smoke.sh            loads the dylib into a real gateway (see below)
README.md                   what it does, config, build, install, how to read the pick log
```

`internal/plugin` takes a `Host` interface (list accounts, reset cooldown, log) and a clock, so every method is tested without cgo. `main.go` wires the real host and holds nothing else. Use the scheduler, usage and host-callback-auth-files examples under `examples/plugin/` as the model for the glue, the envelopes and the registration JSON. Check the exact JSON field names the host sends for `SchedulerPickRequest` and `UsageRecord`, and the capability key names, in `sdk/pluginapi`, `sdk/pluginabi` and `internal/pluginhost/rpc_schema.go`. Do not guess them.

Every exported entry point recovers from a panic and answers unhandled or an error envelope. A panic must not reach the gateway.

## Tests

Tests come first for each package, and every line of Go outside `main.go` has a test that would fail if the behaviour changed.

| Package | What the tests prove |
|---|---|
| `quota` | Unix and RFC 3339 reset values parse. Header name matching ignores case. A missing or empty header is reported absent. A garbage or past value is present with no reset. |
| `state` | Observe sets `reset7d`, clears it on an unusable header and keeps it when the header is absent. Probe flags set and clear. A `reset7d` that passes makes the account due again. A pending probe blocks a second probe for two minutes, then lapses; an answer inside that time clears it. Concurrent use under `go test -race`. |
| `pick` | Earliest weekly reset wins. Ties break by priority then ID. An unobserved or probe-due candidate is tried first, once. No Claude candidates gives unhandled. Mixed providers ignore the others. Empty candidate list gives unhandled. |
| `retest` | The first pass is due at once, then once per interval. Calls reset for each Claude account and no other. Marks only accounts whose last answer was a 429. A host error on one account does not stop the rest. Interval 0 never runs. |
| `plugin` | Register returns the right schema version, capabilities and metadata. The first retest runs once at the first tick after register, never with interval 0. Reconfigure keeps state. A usage record then a pick gives the expected account, using request JSON copied from the host's own encoding. Malformed JSON and unknown methods give an envelope and no panic. A panicking host is contained. |

Two more checks, as scripts:

- `scripts/build.sh` builds the dylib with `-buildmode=c-shared` and fails on any vet or test failure.
- `scripts/smoke.sh` builds the gateway from the CLIProxyAPI v8.0.51 source named by `CPA_SRC`, starts it on a spare local port with a temp config and this plugin enabled, and asserts the gateway reports the plugin loaded with both capabilities. Then it stops the gateway. This covers `main.go`. The auth dir holds one made-up OAuth file so the retest has a file-backed account to reset.

Stretch, only if it takes under an hour to wire: an end-to-end test with fake credentials pointed at a local fake upstream that returns the rate-limit headers, asserting the pick order through the real gateway. If the Claude executor cannot be pointed at a fake upstream, say why in the README and stop.

## Done when

- `go vet ./...` and `go test -race ./...` pass.
- `scripts/build.sh` produces `plugins/darwin/arm64/soonest-reset.dylib`.
- `scripts/smoke.sh` passes against a gateway built from v8.0.51.
- The README covers config, build, install on the gateway host, rollback (`plugins.enabled: false` and restart) and the pick log.
- Work is committed in small commits on `main`. Commit messages hold the change only: no co-author trailers, no tool bylines.

## Out of scope

- Installing on the gateway host or touching a running gateway in any way. That is a separate step after review.
- A session map, persisted state, or ranking by the 5-hour window.
- Providers other than `claude`.
