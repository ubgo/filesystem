package videoplatform

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// Webhook returns the http.Handler a platform's webhook URL points at. It
// accepts POST only, reads at most MaxWebhookBytes, lets the Platform
// verify the signature (403 when it fails, before anything is read from
// the database; 400 for a correctly signed body it cannot read), and
// records the asset's state (apply). A request replayed within
// WebhookTolerance is accepted and changes nothing: the asset already
// holds the state it reports, so no row, status or event changes, and
// a stale "preparing" never undoes a later state. Acknowledged
// with 204: an applied event, one that changes nothing, an event that is
// not about an asset, and an asset no row knows (one created outside the
// app, or a race with Publish's commit, which Sync repairs), so the
// platform does not retry them for a day. A database failure is 503, so
// the platform retries. log receives what the client is not told; nil
// discards it.
func (p *Plugin) Webhook(s *records.Store, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
		if err != nil {
			http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		asset, err := p.platform.Webhook(r, body)
		switch {
		case errors.Is(err, ErrIgnoredEvent):
			w.WriteHeader(http.StatusNoContent)
			return
		case filesystem.CodeOf(err) == filesystem.CodeInvalidArgument:
			log.WarnContext(r.Context(), "videoplatform: webhook unreadable", "platform", p.platform.Name(), "err", err)
			http.Error(w, "unreadable webhook", http.StatusBadRequest)
			return
		case err != nil:
			log.WarnContext(r.Context(), "videoplatform: webhook refused", "platform", p.platform.Name(), "err", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if _, err := p.apply(r.Context(), s, asset); err != nil && filesystem.CodeOf(err) != filesystem.CodeNotFound {
			log.ErrorContext(r.Context(), "videoplatform: webhook not recorded", "platform", p.platform.Name(), "asset", asset.ID, "err", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
