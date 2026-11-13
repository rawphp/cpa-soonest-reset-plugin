# soonest-reset

A scheduler and usage plugin for the CLIProxyAPI gateway, v8.0.51 on darwin/arm64.

It does two things for Claude accounts:

1. It sends each request to the account whose weekly quota resets first. Quota left unused at a reset is lost, so that account should be spent first.
2. Once an hour it clears the gateway's cooldown records for every Claude account. On a 429 the gateway records a cooldown until the reset time in the response headers and never tests the account again before then. When the provider returns quota early, that record goes stale and blocks an account that has quota, sometimes for days.

Other providers are left to the gateway's own scheduler.

## How it picks

The plugin learns each account's weekly reset from the `Anthropic-Ratelimit-Unified-7d-Reset` response header, which the gateway passes to `usage.handle` with every usage record. A usable future value replaces the stored reset. A past or unparsable value clears it. A response without the header, such as a 500, 529 or 401, leaves the stored reset alone, so one transient failure cannot push an account to the back of the order. State lives in memory only. After a gateway restart every account is unknown.

On each `scheduler.pick` it looks only at Claude candidates, across all priority tiers:

- An account is due a try when the plugin has not seen it answer yet, when its weekly reset has passed, or when the retest asked for it. A due account goes first, highest `priority` then lowest ID. It is then marked pending, so a second request does not try it again before the first answers. If no answer arrives within two minutes, the mark lapses and the account is due again.
- A token count (`/v1/messages/count_tokens`) never takes a probe. The gateway selects an account for it but publishes no usage record, so a probe spent on it would get no answer. It follows the order below.
- Otherwise the earliest weekly reset wins. Ties go to the higher `priority`, then the lower ID. An account with no known reset comes after every account that has one.

When there is no Claude candidate, or anything goes wrong, the plugin answers "unhandled" and the gateway's fill-first scheduler picks. It never rejects a pick. Exhausted accounts need no tracking, because the gateway cools them and leaves them out of the candidates.

## The retest

Every `retest_minutes` the plugin calls `host.auth.list`, then `host.routing.reset_cooldown` for each Claude account. An account whose last answer was a 429 is then due a try. If it is still exhausted it answers 429 again, the gateway cools it with a current reset time, and the same request moves to the next account. That costs one failed attempt per such account per interval.

The first pass runs at the ticker's first tick, a minute after the plugin registers. The gateway restores cooldown records after a restart, so a stale one would otherwise block an account for a full interval. The pass does not run inside `plugin.register`, because the gateway registers plugins before it binds its auth manager.

The retest runs on a ticker goroutine that checks once a minute. It starts at `plugin.register` and stops at `plugin.quiesce` or shutdown. It does not run inside `scheduler.pick`. The host does allow callbacks from there, but it only calls the scheduler when at least one account is available. If every Claude account is cooled, no pick happens, and a retest tied to picks would never run. Every request would then fail with `503 auth_unavailable` until the stale records expired on their own.

The retest only reaches accounts that `host.auth.list` returns, which are the file-backed accounts in the auth directory. Claude API keys configured in `config.yaml` are not in that list, so their cooldowns are not reset. They are still ordered by weekly reset.

## Config

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

`retest_minutes` defaults to 60 and `log_picks` to true. `plugin.reconfigure` applies a new config and keeps the account state. An invalid config is refused and the old one stays.

## Build

```bash
./scripts/build.sh
```

It runs `go vet ./...` and `go test -race ./...`, then builds `plugins/darwin/arm64/soonest-reset.dylib` with `-buildmode=c-shared`. The dylib is stamped for macOS 13.0 or later, and `MACOS_MIN` changes that. Without the stamp, clang marks it for the build Mac's macOS version and an older Mac refuses to load it.

```bash
git clone --depth 1 --branch v8.0.51 https://github.com/router-for-me/CLIProxyAPI.git /path/to/CLIProxyAPI-v8.0.51
CPA_SRC=/path/to/CLIProxyAPI-v8.0.51 ./scripts/smoke.sh
```

`CPA_SRC` points at the CLIProxyAPI v8.0.51 source, either a clone of the `v8.0.51` tag or the unpacked source archive from that release. The script stops with a message when it is unset or not a CLIProxyAPI source directory. `GATEWAY_BIN` can name an already built gateway instead. The script builds the gateway from that source, starts it on a free localhost port with a temp config, and sends requests through two made-up Claude API keys to the fake upstream in `scripts/fake_upstream.py`. It checks that:

- the gateway registers the plugin
- a token count is picked without using up a probe
- the first two picks are probes, then picks follow the soonest reset, which proves both `scheduler.pick` and `usage.handle` work
- a 429 moves the same request to the other account
- the first retest runs at startup and resets a cooldown through the real host callbacks

It takes about a minute and a half, most of it waiting for the first retest. It needs Go and Python 3, and stops the gateway when done. The gateway itself checks npm and antigravity versions over the network at startup. No request goes to Anthropic.

## Install

The gateway host needs no Go. Copy the built dylib.

1. Check the gateway is v8.0.51 and the host runs macOS 13 or later on arm64.
2. Copy `plugins/darwin/arm64/soonest-reset.dylib` to `<plugins.dir>/darwin/arm64/soonest-reset.dylib`. `plugins.dir` defaults to `plugins`, relative to the gateway's working directory. The file name sets the plugin ID, so keep it.
3. Add the config block above to the gateway's `config.yaml`.
4. Restart the gateway.
5. Check the gateway log for `pluginhost: plugin registered plugin_id=soonest-reset`, then for `soonest-reset: pick` lines once traffic flows.

## Rollback

Set `plugins.enabled: false` and restart the gateway. `plugins.configs.soonest-reset.enabled: false` works too. Fill-first comes back at once. The plugin writes nothing to disk, so nothing else needs undoing.

## The pick log

With `log_picks: true` each pick writes one info line:

```
soonest-reset: pick account=claude-acct-b@example.test.json rule=soonest-reset reset_7d=2026-11-18T00:00:00Z model=claude-opus-4-5
```

- `account` is the gateway's auth ID.
- `rule` is `probe` when the account was due a try, otherwise `soonest-reset`.
- `reset_7d` is the weekly reset the plugin knew at the time, or `none`.

Each retest writes one line, at warn level when a host call failed:

```
soonest-reset: retest reset=4 probe=[claude-acct-a@example.test.json]
```

`reset` counts the accounts whose cooldown was cleared. `probe` lists those the next picks will try.

The gateway's log formatter prints only fields it knows, so the plugin puts these facts in the message text.

## Session affinity

When the plugin handles a pick, the gateway skips its session-affinity selector. The order is stable, so a session stays on one account until that account is exhausted or a reset passes. A probe moves one request to another account, which costs one prompt-cache miss. There is no session map in this version. Compare the cache-read rate before and after install, and use the pick log to line up account switches.

## Layout

| Path | What it holds |
|---|---|
| `main.go` | cgo glue only: init, call, free, shutdown, host call |
| `internal/quota` | header parsing to a weekly reset time |
| `internal/state` | per-account state, safe for concurrent use |
| `internal/pick` | the pick rule as a pure function |
| `internal/retest` | the retest pass and its schedule |
| `internal/plugin` | method dispatch and the host callback client |

The tests in `internal/plugin` decode request JSON in `testdata/`, written by `json.Marshal` of the gateway's own v8.0.51 types.
