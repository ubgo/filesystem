package recordshttp

import (
	"context"
	"net/http"

	"github.com/ubgo/filesystem"
)

type scopeKey struct{}

// WithScope returns ctx carrying the scope (tenant, workspace) the request
// acts in. The application's authentication middleware sets it once, in
// whatever router it uses, and Config.Scope reads it back with
// ScopeFromRequest, so every framework adapter shares this one key
// instead of carrying its own. "" is a scope like any other (an
// application without tenants); a context without WithScope has none.
func WithScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeFromContext returns the scope set with WithScope, and whether one
// was set.
func ScopeFromContext(ctx context.Context) (string, bool) {
	scope, ok := ctx.Value(scopeKey{}).(string)
	return scope, ok
}

// ScopeFromRequest is a ready Config.Scope: the scope set on the request's
// context with WithScope. A request without one is refused (Handler
// answers 401), so a route mounted outside the authentication middleware
// fails closed instead of acting in the "" scope.
func ScopeFromRequest(r *http.Request) (string, error) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		return "", filesystem.Errorf(filesystem.CodePermission, "recordshttp: no scope on the request (WithScope)")
	}
	return scope, nil
}
