# Advanced usage

Beyond a single `myrtille run` against an already-running service: having myrtille manage the
service's own lifecycle, running several scenarios as a suite, and seeding state without
`init.steps`.

## Starting and stopping the service (`service.managed`)

By default `myrtille run` assumes the service under test is already running at `service.base_url`
and never touches it — an **external** service, in whatever way you already run it (a long-lived
dev server, a remote staging deployment, a container started outside myrtille entirely). Setting
`service.managed` instead makes `myrtille run` launch the service itself — start, wait for
readiness, run everything else, stop — for a fresh instance per run (no leftover heap/GC/DB-pool
state from a previous run bleeding into the next one's measurements). The two are mutually
exclusive by construction: a config either omits `service.managed` (external, the default) or
sets it (managed) — there's no third, half-configured state:

```yaml
service:
  base_url: http://localhost:8082
  managed:
    start_command: ./scripts/dev-server   # sh -c, same env-inheritance rules as init.command
    stop_signal: TERM                      # optional, this is the default
    stop_timeout: 30s                      # optional, this is the default
    readiness:
      url: /healthz                        # required — resolved against base_url
      timeout: 5m                          # optional, this is the default
      interval: 1s                         # optional, this is the default
```

> **Migrating from an older myrtille version:** `start_command`/`stop_signal`/`stop_timeout`/
> `readiness`/`log_file` used to sit directly under `service:`. They've all moved under
> `service.managed:` (same names, same meaning) — a config still using the old flat layout fails to
> load with a clear `service.<field> has moved under service.managed` error rather than silently
> doing nothing.

`stop_signal` accepts `TERM`, `INT`, `HUP`, `QUIT`, or `KILL`. `readiness.url` is the only field
`service.managed` actually requires — everything else in the block has a default.

`start_command` runs via `sh -c`, inheriting the process environment exactly like `init.command`/
`k6.script`. Its own stdout/stderr are captured (not streamed live — the service runs in parallel
with the rest of the pipeline, so an interleaved stream would be unreadable); on a readiness
timeout or an early non-zero exit, the last lines of that captured output are included in the
error, to help diagnose why it never came up. A **clean exit (code 0) before readiness is not
itself a failure** — it's the expected shape for a launcher that backgrounds the real server and
exits itself (`./start.sh &`-style); only a non-zero exit fails fast.

By default that captured output lives in a throwaway temp file, deleted once the service stops —
only the last lines survive, and only on failure. Setting `service.managed.log_file` keeps the
whole thing instead, at a path of your choosing (resolved relative to the config file, like
`k6.script`):

```yaml
service:
  managed:
    log_file: ./logs/service.log
```

`myrtille run` prints `service log: <path>` once the file is created, and the file is overwritten
at the start of every run (not appended across runs) — it always reflects only the most recent run,
whether that run succeeded or failed.

The launched command is tracked by its whole **process group**, not just its own direct PID —
`myrtille` sends `stop_signal` to the group, so a launcher that backgrounds a child and exits
immediately still gets cleaned up correctly (verified against a real forking launcher). The one
case this doesn't cover: a launcher that explicitly detaches its child via `setsid` escapes the
group entirely and won't be stopped — not supported in this version; avoid `setsid`-style
detachment in `start_command` if you need `myrtille` to be able to stop it.

Stopping is **best-effort**, like `teardown.steps`: if the service doesn't stop within
`stop_timeout`, the run still completes (the report's Service section shows "TIMED OUT" instead of
"CLEAN"), and stopping still happens after a failed k6 run, not just a successful one. It runs
after `teardown.steps`, if configured, so teardown can still reach the service. Without
`service.managed` (the external case), the report has no Service section at all — there's nothing
myrtille started or stopped to report on; a service's own `/metrics` still feeds the live
dashboard regardless (see [Live dashboard](live-dashboard.md)), independently of whether myrtille
manages its lifecycle.

If something is already answering `readiness.url` when `start_command` is about to run, the run
fails immediately with a clear error rather than starting a second instance on top of it or
silently reusing it — myrtille doesn't try to guess whether the existing process is "ours".

## Running a suite of scenarios (`myrtille run --suite`)

A project with several scenarios (smoke, list, write, cascade-update, ...) can run them all in one
command/CI step, each against a freshly restarted service instance, with its own report — instead
of hand-rolling that loop in a wrapper shell script:

```yaml
# suite.yaml
scenarios:
  - benchmark/myrtille/smoke.yaml
  - benchmark/myrtille/perimeter-list.yaml
  - benchmark/myrtille/perimeter-write.yaml
```

```sh
myrtille run --suite suite.yaml
```

`--suite` is mutually exclusive with `--config` (passing both explicitly is a load error). Each
listed path is resolved relative to the suite file's own directory, like `k6.script`. Each scenario
still runs its own full init → k6 → report pipeline, in its own timestamped report directory — a
suite is a driver over today's single-run behavior, not a new report shape.

Every scenario runs as its own, separate `myrtille run` **subprocess** — not an in-process loop.
This matters: `config.Load` merges `.env` files and expands `${VAR}` references by mutating the
process environment, which only ever *adds* variables, never overwrites ones already set — running
multiple scenario configs in one process would let an early scenario's env values silently leak
into a later one's. Re-running as a real subprocess per scenario gives each one the same clean-slate
isolation a separate `myrtille run` invocation already has today. It also means every scenario's
`service.metrics.url` (like any base_url-relative URL — see the [config reference](config-reference.md))
is resolved against its own `service.base_url` independently, exactly like a standalone run — no
suite-level special-casing.

If a scenario has `service.managed` configured, it's restarted between scenarios for
free — every scenario already starts and stops its own service instance independently (see above),
so back-to-back scenarios each get a fresh one with no extra suite-level bookkeeping.

A shared warm instance across the whole suite instead of a restart per scenario:

```yaml
scenarios:
  - benchmark/myrtille/smoke.yaml
  - benchmark/myrtille/perimeter-list.yaml
  - benchmark/myrtille/perimeter-write.yaml
restart_between_runs: false
```

The **first** scenario's `service` block is used to start the shared instance, once, before any
scenario runs — it must configure `service.managed` (a load error otherwise, since there'd
be nothing to share). Every scenario must target the same `service.base_url` (also a load error
otherwise, since a suite sharing one instance across scenarios pointed at different services would
be a silent config mistake). Every scenario then runs with the shared instance already up, without
starting or stopping anything itself; the instance is stopped once, after the whole suite finishes
(best-effort, like a single run's own service shutdown). Only the first scenario needs
`service.managed` (`start_command`/`readiness`/`stop_signal`/`stop_timeout`) — the rest only need a
matching `service.base_url`.

One scenario failing (an init error, a k6 threshold) does **not** stop the rest of the suite —
every scenario still gets a chance to run and report, matching "get every scenario's result from
one CI step" over "stop at the first red". A one-line summary is printed at the end
(`PASS`/`FAIL`, config path, report path, one line per scenario), and the overall `myrtille run
--suite` exit code is non-zero if *any* scenario failed — CI still goes red.

## Loading a preloaded state file (`myrtille run --state-file`)

`init.steps` is a declarative template/count/extract mini-language — it can't express seeding logic
that's too dynamic (e.g. a recursive generator with per-level arithmetic, or nested CIDR
computation from a parent). For that case, seed the state dict entirely outside myrtille (any tool,
any language) and load it directly, skipping `init.steps`:

```sh
myrtille run --config myrtille.yaml --state-file /path/to/preloaded-state.json
```

The file must be the same flat JSON object shape `myrtille init`/`state.Dict` produces
(`{"key": [...]}`). `--state-file` is mutually exclusive with both `init.steps` and `init.command`
(see the [config reference](config-reference.md#running-an-external-setup-script-initcommand)) —
configuring more than one is a hard error. k6 receives the loaded dict exactly as it would
init.steps' output (same `k6.state_env` variable); the report's "Init Phase" section stays empty
(`init: null` in JSON) since no init phase ran.
