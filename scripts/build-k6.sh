#!/usr/bin/env bash
# Builds the custom k6 binary myrtille needs for the live web-dashboard
# integration: bundles pkg/promscrape (k6/x/promscrape), the xk6 extension
# that scrapes the service's /metrics endpoint into k6's own metrics
# pipeline — see docs/plans/xk6-live-dashboard.md. Stock k6 cannot run
# scripts that import k6/x/promscrape.
#
# The k6 version built against is whatever pkg/promscrape/go.mod requires
# (go.k6.io/k6/v2) — xk6 resolves it from there, so bump it by updating that
# go.mod (`cd pkg/promscrape && go get go.k6.io/k6/v2@vX.Y.Z`), not here.
set -euo pipefail

if ! command -v xk6 >/dev/null 2>&1; then
  echo "xk6 not found on PATH. Install it with:" >&2
  echo "  go install go.k6.io/xk6/cmd/xk6@latest" >&2
  exit 1
fi

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${OUT:-${root_dir}/bin/k6}"

# GOOS/GOARCH, if set (same env vars `go build` itself reads), cross-compile
# the custom binary — used by the release workflow to build it once per
# release-matrix arch, next to the myrtille binary for that same arch. Unset
# (plain local `./scripts/build-k6.sh`) falls back to xk6's own defaults
# (linux/amd64), unchanged from before this was added.
os="${GOOS:-linux}"
arch="${GOARCH:-amd64}"

# pkg/promscrape imports internal/metrics (reusing Parse/Sample rather than
# duplicating the Prometheus parsing logic — see the package doc). Its own
# go.mod already has a `replace github.com/thecoons/myrtille => ../..` for
# that, but replace directives in a *dependency's* go.mod are ignored by Go
# — only the main module's replaces apply. xk6's generated build module is
# what's actually "main" here, so the same replace has to be re-declared on
# this command line, or the build tries (and fails) to fetch
# github.com/thecoons/myrtille from the network instead of using this
# checkout.
xk6 build \
  --with "github.com/thecoons/myrtille/pkg/promscrape=${root_dir}/pkg/promscrape" \
  --replace "github.com/thecoons/myrtille=${root_dir}" \
  --os "${os}" \
  --arch "${arch}" \
  --cgo=0 \
  -o "${out}"

echo "built ${out} (${os}/${arch})"
