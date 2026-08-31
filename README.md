# myrtille

`myrtille` orchestrates [k6](https://k6.io) load tests in three phases:

1. **init** — brings the tested service into a known state (creating data via declarative HTTP
   calls) and builds a state dictionary.
2. **run** — launches the k6 scenario, passing it this state dictionary, while periodically
   scraping the service's `/metrics` endpoint (Prometheus format) to observe its behavior under
   load.
3. **report** — writes a report (Markdown, JSON and/or HTML) combining the init summary, the k6
   results (thresholds, percentiles, etc.) and the metrics collected during the run. The HTML
   format adds, for each scraped metric, a [Chart.js](https://www.chartjs.org) chart of its
   evolution over time (plus a bar chart per aggregated k6 metric), with hover tooltips — the
   library is vendored and embedded in the binary (`go:embed`), so the report stays self-contained
   and can be viewed offline, with no network request or CDN.

A single generic CLI binary (`myrtille`), driven by a per-project YAML config file — no Go code
to write on the consuming project's side.

## Requirements

- Go 1.27+
- The [`k6`](https://k6.io/docs/get-started/installation/) binary must be on the `PATH`.

## Installation

```sh
go install github.com/antobarth/myrtille/cmd/myrtille@latest
```

Or locally:

```sh
go build -o bin/myrtille ./cmd/myrtille
```

## Usage

```sh
myrtille run --config myrtille.yaml
myrtille init --config myrtille.yaml   # runs only the init phase, for debugging
```

The exit code of `myrtille run` mirrors k6's (0 = success, 99 = failed thresholds, other = script
error). The report is always written, even if the init phase or the thresholds fail.

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
  script: ./scenario.js
  args: ["--vus", "10", "--duration", "30s"]   # passed through to `k6 run` verbatim
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

### Consuming the state in the k6 script

The state dictionary is serialized to JSON and its path passed to the k6 subprocess via the
environment variable defined by `k6.state_env` (`STATE_FILE` by default). Standard pattern on the
script side:

```js
const state = JSON.parse(open(__ENV.STATE_FILE));
const userId = state.user_ids[Math.floor(Math.random() * state.user_ids.length)];
```

## Full example

See [`examples/demo-service`](examples/demo-service): a minimal HTTP service (`stubservice`), a
`myrtille.yaml` config and a `scenario.js` that exercise it, forming a complete end-to-end smoke
test.

```sh
go build -o /tmp/stubservice ./examples/demo-service/stubservice
/tmp/stubservice &

go build -o bin/myrtille ./cmd/myrtille
bin/myrtille run --config examples/demo-service/myrtille.yaml
```

The report is written to `examples/demo-service/reports/<timestamp>/`.

To see genuinely eventful charts in the HTML report (rather than flat or strictly increasing
curves), see [`examples/inventory-service`](examples/inventory-service): a stub whose metrics
depend on load (queue depth, latency, stock per SKU, error rate) and a `scenario.js` with
ramp-up/ramp-down stages (`stages`) to make them vary.

```sh
go build -o /tmp/inventory-stubservice ./examples/inventory-service/stubservice
/tmp/inventory-stubservice &

go build -o bin/myrtille ./cmd/myrtille
bin/myrtille run --config examples/inventory-service/myrtille.yaml
```
