package tus

import (
	"context"
	"log/slog"

	xslog "golang.org/x/exp/slog"
)

// expHandler lets tusd, which logs through golang.org/x/exp/slog, write to
// a standard library *slog.Logger: Config takes the stdlib type so an app
// passes the logger it already has, and the x/exp types never appear in
// this package's API. Records and attributes are converted one to one.
type expHandler struct{ h slog.Handler }

// tusdLogger wraps l for tusd's handler.Config.Logger.
func tusdLogger(l *slog.Logger) *xslog.Logger {
	return xslog.New(expHandler{h: l.Handler()})
}

func (e expHandler) Enabled(ctx context.Context, l xslog.Level) bool {
	return e.h.Enabled(ctx, slog.Level(l))
}

func (e expHandler) Handle(ctx context.Context, r xslog.Record) error {
	out := slog.NewRecord(r.Time, slog.Level(r.Level), r.Message, r.PC)
	r.Attrs(func(a xslog.Attr) bool {
		out.AddAttrs(stdAttr(a))
		return true
	})
	return e.h.Handle(ctx, out)
}

func (e expHandler) WithAttrs(as []xslog.Attr) xslog.Handler {
	return expHandler{h: e.h.WithAttrs(stdAttrs(as))}
}

func (e expHandler) WithGroup(name string) xslog.Handler {
	return expHandler{h: e.h.WithGroup(name)}
}

// stdAttr converts one attribute, resolving LogValuers (an x/exp
// LogValuer is not a stdlib one) and converting groups recursively.
func stdAttr(a xslog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if v.Kind() == xslog.KindGroup {
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(stdAttrs(v.Group())...)}
	}
	return slog.Any(a.Key, v.Any())
}

func stdAttrs(as []xslog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = stdAttr(a)
	}
	return out
}
