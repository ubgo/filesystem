package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// fakeMux answers the three calls of the Mux Video API the adapter makes
// (create an asset, read it, delete it), the way Mux documents them, and
// builds the webhook Mux would send when the asset is ready. It keeps one
// asset and transcodes nothing.
type fakeMux struct {
	mu sync.Mutex
	// fetched is the input URL the last create named: what Mux would
	// download.
	fetched string
	// passthrough is the reference the asset was created with.
	passthrough string
}

// Identifiers the fake hands out.
const (
	fakeAssetID    = "a1b2c3"
	fakePlaybackID = "pb9x8y7"
	// fakeDuration is the length the ready event reports, in seconds.
	fakeDuration = 12.5
)

// Asset states the fake reports, as Mux names them: an asset is
// "preparing" while Mux transcodes it and "ready" once it can be played.
const (
	muxStatePreparing = "preparing"
	muxStateReady     = "ready"
)

func newFakeMux() *fakeMux { return &fakeMux{} }

// muxAsset is the asset object as Mux writes it.
type muxAsset struct {
	ID          string        `json:"id"`
	Status      string        `json:"status"`
	Duration    float64       `json:"duration,omitempty"`
	Passthrough string        `json:"passthrough,omitempty"`
	PlaybackIDs []muxPlayback `json:"playback_ids"`
}

type muxPlayback struct {
	ID     string `json:"id"`
	Policy string `json:"policy"`
}

func (m *fakeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := r.BasicAuth(); !ok {
		http.Error(w, `{"error":{"type":"unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/video/v1/assets":
		var in struct {
			Inputs      []struct{ URL string } `json:"inputs"`
			Passthrough string                 `json:"passthrough"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Inputs) != 1 {
			http.Error(w, `{"error":{"type":"invalid_parameters"}}`, http.StatusBadRequest)
			return
		}
		m.fetched, m.passthrough = in.Inputs[0].URL, in.Passthrough
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]muxAsset{"data": m.asset(muxStatePreparing)})
	case r.Method == http.MethodGet && r.URL.Path == "/video/v1/assets/"+fakeAssetID:
		_ = json.NewEncoder(w).Encode(map[string]muxAsset{"data": m.asset(muxStateReady)})
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// asset is the fake's asset in state status.
func (m *fakeMux) asset(status string) muxAsset {
	a := muxAsset{ID: fakeAssetID, Status: status, Passthrough: m.passthrough, PlaybackIDs: []muxPlayback{{ID: fakePlaybackID, Policy: "public"}}}
	if status == muxStateReady {
		a.Duration = fakeDuration
	}
	return a
}

// readyEvent is the body of the video.asset.ready webhook for asset id.
func (m *fakeMux) readyEvent(id string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.asset(muxStateReady)
	a.ID = id
	body, _ := json.Marshal(struct {
		Type      string   `json:"type"`
		CreatedAt string   `json:"created_at"`
		Data      muxAsset `json:"data"`
	}{"video.asset.ready", time.Now().UTC().Format(time.RFC3339Nano), a})
	return body
}
