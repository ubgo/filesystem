package upload

import (
	"net/http"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// Limits is what a client needs to prepare a file before sending it: the
// sizes, types and extensions the policy accepts, and, when the server
// checks them, the largest image dimensions, so a phone resizes a photo
// before uploading rather than having it refused after.
type Limits struct {
	MaxBytes   int64    `json:"max_bytes"`
	MinBytes   int64    `json:"min_bytes,omitempty"`
	Allowed    []string `json:"allowed"`
	Denied     []string `json:"denied,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	MaxWidth   int      `json:"max_width,omitempty"`
	MaxHeight  int      `json:"max_height,omitempty"`
}

// LimitsOptions configure LimitsHandler.
type LimitsOptions struct {
	// MaxWidth and MaxHeight publish the image dimensions the server
	// enforces (for instance with an Inspect step that checks them); 0
	// publishes none. They describe the server's check; they do not add
	// one.
	MaxWidth, MaxHeight int
	// Resolve gives each request its own policy, as the upload handlers'
	// Resolve does, so a client sees the limits of its own plan.
	Resolve Resolver
	// Errors renders a failed request; see fshttp.ErrorWriter. nil is
	// JSONErrors.
	Errors fshttp.ErrorWriter
	// Respond renders the Limits; see SuccessWriter. nil is JSONSuccess,
	// today's body unchanged.
	Respond SuccessWriter
}

// LimitsOf returns the limits p enforces, for clients.
func LimitsOf(p Policy, o LimitsOptions) Limits {
	return Limits{
		MaxBytes:   p.MaxSize,
		MinBytes:   p.MinSize,
		Allowed:    p.Allowed,
		Denied:     p.deniedTypes(),
		Extensions: p.Extensions,
		MaxWidth:   o.MaxWidth,
		MaxHeight:  o.MaxHeight,
	}
}

// LimitsHandler answers GET with the JSON Limits of the policy (resolved
// per request when o.Resolve is set), the contract a client follows to
// downscale and filter files before uploading. Mount it beside the upload
// endpoint, behind the same authentication when limits depend on the user.
func LimitsHandler(base Policy, o LimitsOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSONError(o.Errors, w, r, http.StatusMethodNotAllowed, filesystem.Errorf(filesystem.CodeInvalidArgument, "limits are read with GET"))
			return
		}
		p, err := resolve(o.Resolve, r, base)
		if err != nil {
			writeResolveError(o.Errors, w, r, err)
			return
		}
		writeSuccess(o.Respond, w, r, http.StatusOK, LimitsOf(p, o))
	})
}
