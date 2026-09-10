// Package xk6ext bundles myrtille's two xk6 extensions —
// pkg/xk6ext/oteltrace (k6/x/oteltrace) and pkg/xk6ext/promscrape
// (k6/x/promscrape) — under one Go module, so xk6 build --with needs only
// this one package: both extensions register themselves (via
// modules.Register in their own init()) as a side effect of being imported
// here. See scripts/build-k6.sh.
package xk6ext

import (
	_ "github.com/thecoons/myrtille/pkg/xk6ext/oteltrace"
	_ "github.com/thecoons/myrtille/pkg/xk6ext/promscrape"
)
