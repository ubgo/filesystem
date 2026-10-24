// Package layer holds the built-in interceptors: cross-cutting behaviour
// that wraps every Disk operation without wrapping the driver, so no
// capability is ever hidden (see filesystem.Interceptor and ADR-0003).
//
// Install them with filesystem.WithInterceptors; the first one given runs
// outermost. A typical order is
//
//	filesystem.WithInterceptors(
//		layer.Observe(metrics),   // sees the whole operation, retries included
//		layer.Log(logger),
//		layer.Retry(layer.RetryOptions{}),
//		layer.Timeout(10*time.Second), // bounds each attempt
//		layer.Limit(32),
//	)
//
// Interceptors that need third-party libraries (OpenTelemetry, Prometheus)
// live in contrib modules and are built on Observe.
package layer
