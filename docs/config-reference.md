# Config reference (`myrtille.yaml`)

Parsing is strict: an unrecognized key (a typo, or a field from an older `myrtille.yaml` that has
since moved or been renamed) is a load error, not a silent no-op — see the full example below for
every field this schema currently accepts.

```yaml
name: my-service-load-test
ref: "JIRA-PROJ-45"          # optional, informational, shown in the report
env_file: .env               # optional — see "Loading defaults from a .env file" below

# Project-level constants, available in every step's url/body/count
# templates via `.Vars`.
vars:
  user_count: 20
  max_orders_per_user: 3
  domain: "${DOMAIN:-default-domain}"   # optional — see "Referencing environment variables in vars:" below

service:
  base_url: http://localhost:8080
  metrics:
    url: /metrics                         # optional — powers the live dashboard's "Service" tab,
                                           # resolved against base_url like readiness.url; an absolute
                                           # URL (its own scheme/host) also works if the metrics endpoint
                                           # lives elsewhere
    interval: 5s                          # see docs/live-dashboard.md; no effect without a custom k6 binary
  traces:
    enabled: true                         # optional — see docs/live-dashboard.md; no effect without
                                           # a custom k6 binary
  managed:                                # optional — see docs/advanced-usage.md;
                                           # omit entirely for an external service (the default)
    start_command: ./scripts/dev-server
    readiness:
      url: /healthz                       # required whenever service.managed is set

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
  derive:
    # See "Computing derived state (init.derive)" below — runs once, after
    # every init.steps iteration above has finished.
    - as: returning_user_ids
      input: user_ids
      expr: "[.[] | select(. != null)]"

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
  formats: ["markdown", "json"]  # "dashboard-html" also available — see docs/live-dashboard.md
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

## Cleaning up (`teardown.steps`)

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

## Running an external setup script (`init.command`)

`--state-file` (see [Advanced usage](advanced-usage.md#loading-a-preloaded-state-file-myrtille-run---state-file))
still needs an external wrapper (a `run.sh` or CI step) to sequence "seed, then `myrtille run`".
`init.command` instead lets `myrtille run` be the only entry point: it runs a shell command line
(via `sh -c`), inheriting the parent process's environment exactly like `k6.script` — no templating
needed, ordinary env vars (`BASE_URL`, etc.) just work. stdout/stderr are streamed through
myrtille's own, for visibility during a long seed.

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

## Computing derived state (`init.derive`)

`init.steps`' `extract` runs once per HTTP response, so it can't compute anything that needs the
*whole* collection at once — e.g. "every item whose key is never referenced as another item's
parent" is a set difference over every response gathered so far, not a per-response `gjson` path.
`init.derive` runs after `init.steps`/`init.command`/`--state-file` have already produced the state
dict (uniformly across all three — same behavior no matter which one populated it), evaluating a
[jq](https://jqlang.org/) expression (via the embedded, pure-Go
[`itchyny/gojq`](https://github.com/itchyny/gojq) — no external `jq` binary needed) once per rule,
in declaration order:

```yaml
init:
  steps:
    - name: collect_perimeters
      url: "{{.BaseURL}}/domains/domain-{{.Index}}/perimeters"
      count: "{{.Vars.domain_count}}"
      extract:
        - path: items
          as: perimeter_items   # raw items, unfiltered — derive needs the full objects
  derive:
    - as: leaf_keys
      input: perimeter_items    # runs against dict.perimeter_items alone, not the whole dict
      expr: |
        (map(select(.spec.parent != null) | (.spec.parent.domain + "/" + .spec.parent.name))) as $parent_keys
        | [.[] | select(((.metadata.domain + "/" + .metadata.name) as $k | $parent_keys | index($k) | not))
            | {domain: .metadata.domain, name: .metadata.name}]
```

- `as` (required) — the state dict key the result is written to. Unlike `extract`, which
  accumulates (appends) across iterations, `derive` **replaces** whatever was already under that
  key — it computes its result once, from the already-complete collection, so there's nothing to
  accumulate into.
- `expr` (required) — the jq expression, run against `input` (or the whole dict, JSON-encoded, if
  `input` is omitted). It must produce exactly one output value, and that value must be a JSON
  array (wrap the result in `[...]`, as above) — same shape `extract` produces, so `pick`/`random`
  in `k6.steps` work on a `derive`d key exactly as they would on an `extract`ed one.
- `input` (optional) — the name of an existing dict key to run `expr` against, instead of the
  whole dict. Referencing a key that was never populated is a load error (fails fast rather than
  silently deriving from nothing).

Rules run in declaration order against a dict that later rules can still read from — a rule can
reference a key an earlier rule just derived, not only ones `extract` populated. `myrtille init`
(the debug subcommand) runs `derive` too, so a rule can be previewed without a full `myrtille run`.

## Loading defaults from a `.env` file (`env_file`)

Values that vary per developer or per machine (base URL, dataset size, timeouts, feature flags)
can live in a `.env` file next to the config instead of being hardcoded in the YAML or exported by
hand every session:

```yaml
env_file: .env   # optional; resolved relative to this config file's own directory
```

```sh
# .env
BASE_URL=http://localhost:8080
SEED=true
```

or equivalently, without touching the config:

```sh
myrtille run --config myrtille.yaml --env-file .env.local   # resolved relative to the current directory
```

`--env-file` wins entirely over `env_file` when both are set (the two files are never merged).
Loading happens once, before anything reads the process environment — `init.command` and
`k6.script`'s env passthrough (both above) and everything else that reads `os.Environ()` all see
the merged result automatically.

**A variable already exported in the shell is never overwritten by the `.env` file** — only keys
not already present in the process environment get the file's value. This is what lets a one-off
override win without editing or duplicating the file:

```sh
SEED=false myrtille run --config myrtille.yaml   # SEED=false wins even if .env has SEED=true
```

The file itself is a static list of defaults, not a script: plain `KEY=value` lines, `#`-comments,
and blank lines are supported; a single layer of matching quotes (`KEY="value with spaces"`) is
stripped. There's no `$VAR` expansion and no `export` prefix. `env_file` left unset is a silent
no-op; set but pointing at a file that doesn't exist is a load error (fails loudly on a typo rather
than silently running with fewer defaults than intended). `env_file`/`--env-file` isn't a
replacement for `vars:` below — `vars:` stays for values that are part of the scenario's own
definition, not per-environment overrides.

## Referencing environment variables in `vars:`

A `vars:` value can reference the process environment — set directly, or merged in from a `.env`
file (above) — with shell-flavored `${VAR}`/`${VAR:-default}` syntax, expanded once at config-load
time, right after `.env` merging:

```yaml
vars:
  domain: "${DOMAIN}"                        # "" if DOMAIN isn't set
  domain_count: "${DOMAIN_COUNT:-1}"         # falls back to "1" if DOMAIN_COUNT is unset or empty
```

Only string values are expanded — a number or boolean `vars:` entry is left untouched. `:-` follows
shell semantics: the default applies when the variable is unset *or* empty, not only when unset.

## Declarative k6 scenario steps

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

A third function, `uniqueId`, expands to a value guaranteed unique per k6 iteration (`` ${__VU}-
${__ITER}-${Date.now()} ``, or `` ${'setup'}-${0}-${Date.now()} `` inside `k6.setup`, where
`__VU`/`__ITER` aren't defined) — for resource names that must not collide, e.g. `body:
'{"name":"write-{{uniqueId}}"}'`.

`pick` also takes an optional second argument — a field name — to draw a **correlated** value out
of a pool of objects rather than a scalar: `{{pick "perimeter_keys" "domain"}}` and
`{{pick "perimeter_keys" "name"}}` used together in the same step reference the *same* randomly-
chosen element (verified against a real k6 run: paired fields always come from one consistent
element, never mixed across two independent draws), whereas plain `{{pick "pool"}}` calls stay
independent draws, exactly as before. Such an object pool comes from extracting a whole JSON
object per iteration into the same key — e.g. an `init.steps` `extract` with `path: "@this"` —
rather than a single scalar field. The field argument may itself be a dot-separated path
(`{{pick "root_perimeters" "metadata.domain"}}`) to reach into a nested pooled object, not just a
top-level property — see `body_from`/`body_patch` below for a pool of full nested objects.

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

`repeat` (a Go template, like `init.steps`' `count` — resolved once at generation time against
`.BaseURL`/`.Vars`, not at k6 runtime like `pick`/`random`) wraps the step's request in a JS `for`
loop, running it that many times per k6 iteration — for an action that must repeat before the next
step (e.g. touching N perimeters before creating a revision). Omitting it runs the step once, as
before:

```yaml
k6:
  steps:
    - name: touch_perimeter
      method: PATCH
      url: "{{.BaseURL}}/perimeters"
      repeat: "{{.Vars.perimeters_per_version}}"
```

`timeout` (a Go template, same `url`/`body`/`pick`/`random` resolution) overrides k6's fixed 60s
per-request default — for an endpoint that can legitimately take longer (e.g. an unpaginated listing
that scales with dataset size). Omitting it keeps k6's own default, unchanged:

```yaml
k6:
  steps:
    - name: list_all_records
      method: GET
      url: "{{.BaseURL}}/records"
      timeout: "120s"
```

Available on both `k6.steps` and `k6.setup` steps.

### Full-replace PUT with one field changed (`body_from`/`body_patch`)

A full-replace `PUT` that must resend an existing object with only a couple of fields changed
(e.g. touching a `metadata.labels` timestamp while keeping everything else — `spec`, existing
labels, etc. — exactly as it was) can't be expressed with a plain `body` template: there's no way
to say "take this whole pooled object and re-send it with a few fields overridden". `body_from`
picks one full object from a pool and deep-clones it as the step's body; `body_patch` then
overrides specific fields on that clone, by dot-separated path, before it's sent:

```yaml
k6:
  steps:
    - name: touch_root
      method: PUT
      url: '{{.BaseURL}}/domains/{{pick "root_perimeters" "metadata.domain"}}/perimeters/{{pick "root_perimeters" "metadata.name"}}'
      body_from: root_perimeters
      body_patch:
        metadata.labels.touched: "{{uniqueId}}"
      tags:
        updateKind: cascade
      checks:
        "status is 200": "r.status === 200"
```

- `body_from` (a pool name) replaces `body` — mutually exclusive with it. The pick it makes is
  correlated with any other `{{pick "pool" "field"}}` on the *same* pool within the same step
  (like the URL above) — same object, not two independent draws (verified against a real k6 run).
- `body_patch` maps a dot-separated path to a template string (same funcs as `body`/`url` —
  `pick`/`random`/`uniqueId` all usable in a patch value) applied on top of the clone. Paths are
  object nesting only, no array indices. Requires `body_from`.
- The object is deep-cloned before patching — the pool itself is never mutated, so every iteration
  still picks from the original, unpatched objects (verified against a real k6 run across multiple
  iterations: the pool stays byte-identical to its initial state throughout).
- A patch path whose intermediate segment doesn't exist on the picked object throws a real JS
  error at k6 runtime rather than silently creating it (e.g. `metadata.nonexistent.touched` when
  `metadata.nonexistent` isn't on the object) — visible in k6's own output
  (`level=error ... hint="script exception"`), though note this alone doesn't fail the overall k6
  run's exit code (that's k6's own behavior for any script exception, not specific to
  `body_patch` — add a `check` if the run itself must fail on it).

The pool itself needs full objects, not just scalar fields — an `init.steps` `extract` without a
`{...}` projection keeps the whole matched item(s):

```yaml
init:
  steps:
    - name: collect_perimeters
      url: "{{.BaseURL}}/perimeters"
      extract:
        - path: "items.#(spec.parent==~null)#"   # full matching objects, not a {domain,name} projection
          as: root_perimeters
```

## Running something once (`k6.setup`)

`k6.steps` alone has no way to run an HTTP call once for the whole test rather than on every
iteration — e.g. creating a shared resource once, then having every iteration reference it.
`k6.setup` generates a k6 [`setup()`](https://grafana.com/docs/k6/latest/using-k6/test-lifecycle/)
for you, same declarative shape as `init.steps` (with `extract`), run once before any `k6.steps`
iteration:

```yaml
k6:
  setup:
    - name: create_revision
      method: POST
      url: "{{.BaseURL}}/revisions"
      body: '{"name":"bench-{{uniqueId}}"}'
      extract:
        - path: name
          as: version_name
  steps:
    - name: list_by_revision
      url: '{{.BaseURL}}/revisions/{{pick "version_name"}}/items'
```

Extracted values are merged into the same pool `pick`/`random` read from in the regular steps
below — verified with `--http-debug=full` against a real k6 run: the setup call fires exactly once
(on k6's own dedicated setup VU), and every regular-step VU, including ones that never ran setup
themselves, correctly receives the extracted value (k6 runs each VU as a separate isolate; setup's
result reaches them via its return value, not shared memory).

`extract`'s `path` here is a plain dot-separated JS property/array-index path (e.g. `name`,
`items.0.id`) — a deliberately simpler subset of `init.steps`' gjson syntax, with no `#.field`
flatten-map. `k6.setup` steps have no `tags`/`checks`/`sleep`/`repeat` (not meaningful for a
run-once bootstrap call) and are mutually exclusive with `k6.script` — with a hand-written script,
write `setup()` yourself.

Every named check's pass/fail counts (across the whole run, including any declared via a custom
`k6.script`) are read from k6's `--summary-export` output and shown in the report under "Checks" —
a bullet list in Markdown, and `k6.Summary.Checks` in JSON.

## Custom k6 scripts (`k6.script`)

For a scenario `k6.steps` can't express (custom checks beyond a boolean expression, non-HTTP
protocols, multiple scenarios, etc.), point `k6.script` at a hand-written script instead —
mutually exclusive with `k6.steps`/`k6.options`, which then belong in the script itself. The state
dictionary is serialized to JSON and its path passed to the k6 subprocess via the environment
variable defined by `k6.state_env` (`STATE_FILE` by default). Standard pattern on the script side:

```js
const state = JSON.parse(open(__ENV.STATE_FILE));
const userId = state.user_ids[Math.floor(Math.random() * state.user_ids.length)];
```

With `k6.steps`, a configured `service.metrics.url` automatically wires up live scraping of that
endpoint into k6's dashboard (see [Live dashboard](live-dashboard.md)) — myrtille generates the two
lines needed. With `k6.script`, myrtille never rewrites a hand-written script, so add them yourself:

```js
import promscrape from 'k6/x/promscrape';

const scraper = new promscrape.Scraper('http://localhost:8080/metrics'); // module scope, not inside a function

export function setup() {
  scraper.start(5000); // interval in ms — match service.metrics.interval
}
```

`k6/x/promscrape` only exists in the custom k6 binary described in [Live dashboard](live-dashboard.md)
— already found automatically with a release tarball, otherwise set `MYRTILLE_K6_BIN` to point
myrtille at it.

Same pattern for `service.traces.enabled` (see [Mirroring OTel spans](live-dashboard.md#mirroring-otel-spans-servicetraces))
— the two lines myrtille generates for `k6.steps`, to add yourself for `k6.script`:

```js
import oteltrace from 'k6/x/oteltrace';

const receiver = new oteltrace.Receiver(); // module scope, not inside a function

export function setup() {
  receiver.start();
}
```
