![myrtille](assets/banner.jpeg)

# myrtille

`myrtille` orchestrates [k6](https://k6.io) load tests in three phases:

1. **init** — brings the tested service into a known state (creating data via declarative HTTP
   calls) and builds a state dictionary.
2. **run** — launches the k6 scenario, passing it this state dictionary, while periodically
   scraping the service's `/metrics` endpoint (Prometheus format) to observe its behavior under
   load.
3. **report** — writes a report (Markdown, JSON and/or HTML) combining the init summary, the k6
   results (thresholds, percentiles, per-check pass/fail counts, etc.) and the metrics collected
   during the run. The HTML format adds a [Chart.js](https://www.chartjs.org) evolution-over-time
   chart for each scraped metric and each k6 metric alike, with hover tooltips — the k6 side is
   decoded from k6's own built-in [web dashboard](https://github.com/grafana/k6/tree/master/internal/dashboard)
   record stream (`k6 run --out web-dashboard=record=...`, parsed by `internal/k6run` — data only,
   never the dashboard's own AGPL-licensed frontend), so it works generically for any metric,
   including custom ones defined in a script, without a hardcoded per-metric list. Chart.js itself
   is vendored and embedded in the binary (`go:embed`), so the report stays self-contained and can
   be viewed offline, with no network request or CDN.

A single generic CLI binary (`myrtille`), driven by a per-project YAML config file — no Go code
to write on the consuming project's side.

## Requirements

- Go 1.27+
- The [`k6`](https://k6.io/docs/get-started/installation/) binary must be on the `PATH`.

## Installation

```sh
go install github.com/thecoons/myrtille/cmd/myrtille@latest
```

Or locally:

```sh
go build -o bin/myrtille ./cmd/myrtille
```

Or download a prebuilt Linux binary (amd64/arm64) from the
[releases page](https://github.com/thecoons/myrtille/releases):

```sh
tar -xzf myrtille-vX.Y.Z-linux-amd64.tar.gz
sudo mv myrtille-vX.Y.Z-linux-amd64/myrtille /usr/local/bin/
```

## Usage

```sh
myrtille run --config myrtille.yaml
myrtille init --config myrtille.yaml   # runs only the init phase, for debugging
myrtille teardown --config myrtille.yaml --state-file /tmp/myrtille-state-XXXX.json
```

The exit code of `myrtille run` mirrors k6's (0 = success, 99 = failed thresholds, other = script
error). The report is always written, even if the init phase or the thresholds fail.

`myrtille run` always attempts `teardown.steps` (see below) before returning, even if init or k6
failed partway — so if `teardown.steps` is configured, it prints the state file's path to stderr
first: `state file: /tmp/myrtille-state-XXXX.json`. That path is only useful for recovery — if the
process is killed hard enough (`kill -9`) to skip its own cleanup, rerun it standalone with
`myrtille teardown --state-file <that path>`.

### Loading a preloaded state file (`myrtille run --state-file`)

`init.steps` is a declarative template/count/extract mini-language — it can't express seeding logic
that's too dynamic (e.g. a recursive generator with per-level arithmetic, or nested CIDR
computation from a parent). For that case, seed the state dict entirely outside myrtille (any tool,
any language) and load it directly, skipping `init.steps`:

```sh
myrtille run --config myrtille.yaml --state-file /path/to/preloaded-state.json
```

The file must be the same flat JSON object shape `myrtille init`/`state.Dict` produces
(`{"key": [...]}`). `--state-file` is mutually exclusive with both `init.steps` and `init.command`
(below) — configuring more than one is a hard error. k6 receives the loaded dict exactly as it
would init.steps' output (same `k6.state_env` variable); the report's "Init Phase" section stays
empty (`init: null` in JSON) since no init phase ran.

### Running an external setup script (`init.command`)

`--state-file` above still needs an external wrapper (a `run.sh` or CI step) to sequence "seed,
then `myrtille run`". `init.command` instead lets `myrtille run` be the only entry point: it runs a
shell command line (via `sh -c`), inheriting the parent process's environment exactly like
`k6.script` — no templating needed, ordinary env vars (`BASE_URL`, etc.) just work. stdout/stderr
are streamed through myrtille's own, for visibility during a long seed.

```yaml
init:
  command: ./seed.sh
  command_timeout: 5m   # optional, this is the default
```

The command must write a state.Dict-shaped JSON object (same shape as `--state-file`) to the path
given via the `MYRTILLE_STATE_OUTPUT` env var before exiting `0`:

```sh
#!/bin/sh
echo '{"user_ids": ["u1", "u2"]}' > "$MYRTILLE_STATE_OUTPUT"
```

A non-zero exit or exceeding `command_timeout` fails the run, same as an `init.steps` HTTP failure.
`init.command` is mutually exclusive with both `init.steps` and `--state-file`. The report's "Init
Phase" section shows the command, its exit code, and its duration instead of a step table.

## Config (`myrtille.yaml`)

```yaml
name: my-service-load-test
ref: "JIRA-PROJ-45"          # optional, informational, shown in the report

# Project-level constants, available in every step's url/body/count
# templates via `.Vars`.
vars:
  user_count: 20
  max_orders_per_user: 3

service:
  base_url: http://localhost:8080
  metrics:
    url: http://localhost:8080/metrics   # optional — omit to disable scraping
    interval: 5s                          # scrape frequency during the run

init:
  steps:
    # list_products runs first so product_ids is already in the dictionary
    # by the time create_users (and its children) run.
    - name: list_products
      method: GET
      url: "{{.BaseURL}}/products"
      extract:
        - path: "#.id"         # gjson syntax to extract a field from each element of an array
          as: "product_ids"
    - name: create_users
      method: POST
      url: "{{.BaseURL}}/users"
      body: '{"name": "user-{{.Index}}"}'
      count: "{{.Vars.user_count}}"   # literal ("20") or template expression; {{.Index}} available (0-based)
      extract:
        - path: "id"           # gjson syntax applied to the JSON response
          as: "user_ids"       # accumulated into an array in the state dictionary
      # children: run once per parent iteration, with that iteration's JSON
      # response exposed as `.Parent` — for creating dependent resources
      # rather than only independent pools.
      children:
        - name: create_orders_for_user
          method: POST
          url: "{{.BaseURL}}/orders"
          body: '{"userId":"{{.Parent.id}}","productId":"{{pick .Dict.product_ids}}"}'
          count: "{{random 1 .Vars.max_orders_per_user}}"   # 1 to 3 orders per user

k6:
  # Declarative scenario: myrtille generates the k6 script for you (see
  # "Declarative k6 scenario steps" below). Alternative: `script: ./scenario.js`
  # for a hand-written script — mutually exclusive with `steps`/`options`.
  steps:
    - name: place_order
      method: POST
      url: "{{.BaseURL}}/orders"
      body: '{"userId":"{{pick "user_ids"}}","productId":"{{pick "product_ids"}}"}'
      checks:
        "status is 201": "r.status === 201"
      sleep: 200ms
  options:
    vus: 10
    duration: 30s
    thresholds:
      http_req_failed: ["rate<0.01"]
  state_env: STATE_FILE        # name of the env var exposing the state JSON's path

report:
  output_dir: ./reports
  formats: ["markdown", "json", "html"]  # html adds interactive Chart.js charts
```

Each init step: the request is aborted (and the k6 run cancelled) on the first HTTP failure
(status >= 400) or any extraction error — a partially initialized service would make the
subsequent load test unreliable.

`count` is a Go template (`text/template`), resolved once before iterating the step, with access
to:
- `.Vars` — the project's `vars` block;
- `.Parent` — the parsed JSON response of the parent iteration, `nil` for a root step;
- `.Dict` — a read-only snapshot of the values extracted so far (for picking from an existing pool);

and two functions: `random min max` (an inclusive random integer) and `pick list` (a random
element from a list, typically `.Dict.my_pool`). These same variables/functions are also
available in the `url` and `body` templates. Steps can be nested to any depth via `children`;
reports render the resulting tree as an indented list (`↳`).

### Cleaning up (`teardown.steps`)

`teardown.steps` uses the exact same shape as `init.steps`, run after k6 to remove whatever
`init.steps` created. Go's `text/template` ships `index`/`len` as builtins, so no new syntax is
needed to target exactly what got created — just walk `.Dict` by position:

```yaml
teardown:
  steps:
    - name: delete_users
      method: DELETE
      url: "{{.BaseURL}}/users/{{index .Dict.user_ids .Index}}"
      count: "{{len .Dict.user_ids}}"
```

Unlike `init.steps`, teardown never aborts on a failure (e.g. deleting something already gone) —
it's best-effort by nature, so one failed request doesn't stop the rest of the cleanup. It runs
automatically at the end of every `myrtille run`, on every exit path (success, a failed init, a
failed k6 run) — teardown failures are reported separately (see `myrtille run`'s report) rather
than failing the run itself.

### Declarative k6 scenario steps

For a scenario that's just "hit these endpoints, pick from an extracted pool, sleep a bit",
`k6.steps` generates the k6 script for you — no `scenario.js` to write. It's mutually exclusive
with `k6.script`:

```yaml
k6:
  steps:
    - name: place_order
      method: POST
      url: "{{.BaseURL}}/orders"
      body: '{"userId":"{{pick "user_ids"}}","productId":"{{pick "product_ids"}}"}'
      checks:
        "status is 201": "r.status === 201"   # `r` is the k6 response, as in check(res, {...})
      sleep: 200ms
  options:
    vus: 10
    duration: 30s
    thresholds:
      http_req_failed: ["rate<0.01"]
```

Steps run once each, in declaration order, per k6 iteration — the repetition axis here is
`k6.options` (`vus`/`duration`/`iterations`/`stages`), not a per-step count. `pick`/`random`
mirror the init-phase functions by name, but resolve differently: since k6, not myrtille, drives
a scenario's iteration loop, they can't be evaluated once at generation time — instead they
expand to small JS snippets that the generated script evaluates itself, fresh on every k6
iteration. The generated script is an ephemeral temp file, removed once the run finishes.
`headers` (a map, like `init.steps`) is also supported per step.

A third function, `uniqueId`, expands to a value guaranteed unique per k6 iteration
(`` ${__VU}-${__ITER}-${Date.now()} ``) — for resource names that must not collide, e.g. `body:
'{"name":"write-{{uniqueId}}"}'`.

`tags` (a map, values are templates like `url`/`body`) is passed straight through to the
generated `http.request(..., { tags: {...} })` call, so a step's request metrics can be segmented
by logical variant of the same scenario — e.g. distinguishing two endpoints hit by the same script,
or a `live` vs `revision` selector — the same way a hand-written `k6.script` would tag requests
itself:

```yaml
k6:
  steps:
    - name: get_perimeter
      method: GET
      url: "{{.BaseURL}}/perimeter"
      tags:
        endpoint: get
  options:
    thresholds:
      "http_req_duration{endpoint:get}": ["p(95)<300"]
```

Every named check's pass/fail counts (across the whole run, including any declared via a custom
`k6.script`) are read from k6's `--summary-export` output and shown in the report under "Checks" —
a bullet list in Markdown, a table in HTML, and `k6.Summary.Checks` in JSON.

### Custom k6 scripts (`k6.script`)

For a scenario `k6.steps` can't express (custom checks beyond a boolean expression, non-HTTP
protocols, multiple scenarios, etc.), point `k6.script` at a hand-written script instead —
mutually exclusive with `k6.steps`/`k6.options`, which then belong in the script itself. The state
dictionary is serialized to JSON and its path passed to the k6 subprocess via the environment
variable defined by `k6.state_env` (`STATE_FILE` by default). Standard pattern on the script side:

```js
const state = JSON.parse(open(__ENV.STATE_FILE));
const userId = state.user_ids[Math.floor(Math.random() * state.user_ids.length)];
```

## Full example

See [`examples/demo-service`](examples/demo-service): a minimal HTTP service (`stubservice`) and a
`myrtille.yaml` config exercising it end-to-end (init steps + declarative `k6.steps`, no
hand-written script), forming a complete smoke test.

```sh
go build -o /tmp/stubservice ./examples/demo-service/stubservice
/tmp/stubservice &

go build -o bin/myrtille ./cmd/myrtille
bin/myrtille run --config examples/demo-service/myrtille.yaml
```

The report is written to `examples/demo-service/reports/<timestamp>/`.

## License

MIT — see [LICENSE](LICENSE).
