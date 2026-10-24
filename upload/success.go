package upload

import (
	"encoding/json"
	"net/http"
)

// SuccessBody is a body this package's handlers answer a successful request
// with. The set is closed, so a SuccessWriter can switch on it exhaustively:
//
//   - Response, from Handler (201, or 200 when a Partial request stored
//     some files; a Partial request that stored none answers its Response
//     with the first failure's status, because the failures are a result,
//     not an error) and from Chunked when the last chunk completes a file
//     (201);
//   - ChunkProgress, from Chunked for every other chunk (200);
//   - Ticket, from PresignHandler (200);
//   - Limits, from LimitsHandler (200).
//
// The Uppy and tus protocol bodies are not in the set: their clients parse
// them as the protocols define, so they cannot be reshaped.
type SuccessBody interface {
	successBody()
}

func (Response) successBody()      {}
func (ChunkProgress) successBody() {}
func (Ticket) successBody()        {}
func (Limits) successBody()        {}

// SuccessWriter renders a successful response for a handler of this
// package: the opt-in seam for an API whose responses follow one envelope
// ({data: ...}, JSON:API, camelCase keys), as fshttp.ErrorWriter is for
// errors. status is the one the default answers with; body is one of the
// SuccessBody types. It is called before anything is written, and it owns
// the status line, the Content-Type and the body.
//
// Invariant: a nil SuccessWriter in an options struct means JSONSuccess,
// whose output is byte for byte what every handler answered before the
// seam existed (Uppy's, the upload JSON contract and clients built on
// it keep working). With HandlerOptions.Idempotency a replayed response
// returns the recorded body under the Content-Type the first response set
// (Recorded.ContentType), so an envelope in another media type replays as
// itself; only the status becomes 200.
type SuccessWriter func(w http.ResponseWriter, r *http.Request, status int, body SuccessBody)

// JSONSuccess is the default SuccessWriter: body as JSON with status.
func JSONSuccess(w http.ResponseWriter, _ *http.Request, status int, body SuccessBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeSuccess answers body with status through sw, JSONSuccess when nil.
func writeSuccess(sw SuccessWriter, w http.ResponseWriter, r *http.Request, status int, body SuccessBody) {
	if sw == nil {
		sw = JSONSuccess
	}
	sw(w, r, status, body)
}
