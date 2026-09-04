# Live dashboard

k6 ships its own live web dashboard (VUs, request rate, latencies, thresholds, and any custom
metric). myrtille can additionally mirror the tested service's own `/metrics` endpoint into the
same dashboard, in its own "Service" tab, updating in real time next to k6's own metrics.

This needs a custom k6 binary — stock k6 doesn't have the extension that does the mirroring.
`myrtille` looks for one in this order: `$MYRTILLE_K6_BIN` if set, then a `k6` sitting in the same
directory as the running `myrtille` binary itself, then falls back to plain `k6` on the `PATH`
(stock, no live dashboard).

**Using a release tarball** (see the main [README](../README.md#installation)): already covered,
nothing to do — the bundled `k6` sits right next to `myrtille`, so the second resolution step finds
it automatically. `myrtille` prints `k6 binary: ... (bundled next to myrtille)` to stderr whenever
this kicks in, so it's clear which binary is running.

**Using `go install` or a local build**: no bundled binary to find, so build the custom one once
and point myrtille at it explicitly:

```sh
go install go.k6.io/xk6/cmd/xk6@latest
export PATH="$PATH:$(go env GOPATH)/bin"
./scripts/build-k6.sh   # builds bin/k6

MYRTILLE_K6_BIN="$(pwd)/bin/k6" myrtille run --config myrtille.yaml
```

(If `myrtille` and `bin/k6` happen to live in the same directory — e.g. after
`go build -o bin/myrtille` — the co-located lookup finds it too, `MYRTILLE_K6_BIN` isn't even
required; setting it explicitly always takes priority regardless.)

k6 prints the dashboard's URL to stdout on its own (`web dashboard: http://127.0.0.1:XXXXX`) — open
it in a browser while the run is in progress; myrtille doesn't launch a browser for you. With none
of the three resolution steps finding a custom binary, nothing changes: no live dashboard, stock k6
behavior exactly as before.

**Leaving the dashboard tab open past the end of a run is safe** — k6's own live-dashboard shutdown
has a known bug (as of k6 v2.2.0) where it waits indefinitely for every open browser tab to
disconnect before the process can exit, with no timeout of its own; left unattended, that hangs k6
forever and the report never gets written. myrtille works around it: once the dashboard itself
reports the run as finished, k6 gets a 5s grace period to exit on its own before myrtille kills it
directly, printing a message to that effect and still producing a report (though one killed this way
won't have k6's own metrics summary — the run itself is marked failed, since it didn't shut down
cleanly).

With `k6.steps`, a configured `service.metrics.url` (see the [config reference](config-reference.md))
is wired into the dashboard automatically — every distinct metric family found on that endpoint gets
its own chart in the "Service" tab. With a hand-written `k6.script`, wire it in yourself — see
[Custom k6 scripts](config-reference.md#custom-k6-scripts-k6script).

## Exporting the dashboard to `report.html`

Adding `"dashboard-html"` to `report.formats` (see the [config reference](config-reference.md)) saves
k6/xk6-dashboard's own standalone export — the same dashboard, self-contained (no network access
needed to view it), as `report.html` next to `report.md`/`report.json` in the report directory. This
still requires the custom k6 binary above; requesting it without one fails the run with a clear error
rather than silently producing a report without the file.

## Mirroring OTel spans (`service.traces`)

If the tested service exports [OpenTelemetry](https://opentelemetry.io/) traces, myrtille can
receive them during the run and mirror them into the same live dashboard — `svc_span_duration` (a
Trend, in ms) and `svc_span_errors` (a Rate) rather than one metric per span name, so there's
nothing to list or configure per span. Every sample is tagged `span_name` (and `otel_service`, when
a span's resource carries a `service.name` attribute), so a specific span's numbers are still
addressable — e.g. `k6.options.thresholds["svc_span_duration{span_name:check_inventory}"]` — in the
end-of-run summary and report; the live dashboard panel itself only plots the aggregate across every
span, not broken down by tag (a k6 web-dashboard limitation, not a myrtille one — its live metric
feed doesn't track arbitrary custom-tag submetrics, only thresholds/the final summary do):

```yaml
service:
  traces:
    enabled: true
```

This also needs the custom k6 binary (see above) — `./scripts/build-k6.sh` bundles this extension
alongside the metrics-mirroring one in the same `bin/k6`, so nothing extra to build. Once enabled,
myrtille listens on the standard OTLP/HTTP port (`:4318`, `POST /v1/traces`) for the duration of the
run; point the service's own OTel SDK at it the same way you'd point it at a real collector (most
SDKs already default to `localhost:4318`, so often nothing to change on that side either). Only
OTLP/HTTP is supported — an SDK that only exports via gRPC (port 4317) needs reconfiguring to use
the HTTP exporter instead.

**Most OTel SDKs default to *https*, even with no endpoint configured at all** (confirmed against
the Go SDK: `otlptracehttp.New(ctx)` with zero options tries `https://localhost:4318`, not `http://`
— see `examples/demo-service/stubservice/tracing.go`) — the receiver here only ever speaks plain
HTTP, so this needs turning off explicitly (`otlptracehttp.WithInsecure()` in Go; check the
equivalent for other SDKs/languages) or every export silently fails a TLS handshake against a plain
HTTP server, indistinguishable at a glance from nothing listening at all.

With `k6.steps`, a configured `service.traces.enabled` is wired into the dashboard automatically,
the same way `service.metrics.url` is. With a hand-written `k6.script`, wire it in yourself — see
[Custom k6 scripts](config-reference.md#custom-k6-scripts-k6script).
