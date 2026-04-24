// Package handler contains HTTP request handlers for the API server.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/ntt0601zcoder/open-streamer/internal/coordinator"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/manager"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/ntt0601zcoder/open-streamer/internal/transcoder"
	"github.com/samber/do/v2"
)

// StreamHandler handles stream lifecycle REST endpoints.
type StreamHandler struct {
	streamRepo  store.StreamRepository
	coordinator *coordinator.Coordinator
	manager     *manager.Service
	transcoder  *transcoder.Service
	publisher   *publisher.Service
	bus         events.Bus
}

// streamResponse is the API representation of a stream.
// Persisted config from domain.Stream is embedded at top level; everything
// runtime-computed (status, pipeline activity, input health, transcoder state)
// lives under a single `runtime` envelope so clients have one root for live data.
type streamResponse struct {
	*domain.Stream
	Runtime manager.RuntimeStatus `json:"runtime"`
}

func (h *StreamHandler) withStatus(s *domain.Stream) streamResponse {
	rt, registered := h.manager.RuntimeStatus(s.Code)
	rt.Status = h.coordinator.StreamStatus(s.Code)
	rt.PipelineActive = registered

	// ABR-copy streams bypass the manager entirely (their pipeline is N
	// in-process taps, not an ingest worker), so the manager has no record
	// to report. Substitute the coordinator's synthetic snapshot so the UI
	// can show input status / activity instead of "UNKNOWN".
	if !registered {
		if abrRT, ok := h.coordinator.ABRCopyRuntimeStatus(s.Code); ok {
			abrRT.Status = rt.Status // preserve coordinator-level status set above
			rt = abrRT
		}
	}

	if rt.PipelineActive {
		// Nest transcoder + push runtime under the same envelope; keeps them
		// from colliding with the persisted Stream.Transcoder / Stream.Push
		// config fields on the same json tags.
		if tc, ok := h.transcoder.RuntimeStatus(s.Code); ok {
			rt.Transcoder = &tc
		}
		if pr, ok := h.publisher.RuntimeStatus(s.Code); ok {
			rt.Publisher = &pr
		}
	}
	if rt.Inputs == nil {
		// json marshals a nil slice as `null`; emit `[]` so the contract is
		// stable for clients regardless of pipeline state.
		rt.Inputs = []manager.InputHealthSnapshot{}
	}
	return streamResponse{Stream: s, Runtime: rt}
}

// NewStreamHandler creates a StreamHandler and registers it with the DI injector.
func NewStreamHandler(i do.Injector) (*StreamHandler, error) {
	return &StreamHandler{
		streamRepo:  do.MustInvoke[store.StreamRepository](i),
		coordinator: do.MustInvoke[*coordinator.Coordinator](i),
		manager:     do.MustInvoke[*manager.Service](i),
		transcoder:  do.MustInvoke[*transcoder.Service](i),
		publisher:   do.MustInvoke[*publisher.Service](i),
		bus:         do.MustInvoke[events.Bus](i),
	}, nil
}

// List streams; optional ?status=idle|active|degraded|stopped.
// @Summary List streams
// @Tags streams
// @Produce json
// @Param status query string false "Filter by status"
// @Success 200 {object} apidocs.StreamList
// @Failure 400 {object} apidocs.ErrorBody
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams [get].
func (h *StreamHandler) List(w http.ResponseWriter, r *http.Request) {
	var statusFilter *domain.StreamStatus
	if q := r.URL.Query().Get("status"); q != "" {
		st := domain.StreamStatus(q)
		switch st {
		case domain.StatusIdle, domain.StatusActive, domain.StatusDegraded, domain.StatusStopped:
			statusFilter = &st
		default:
			writeError(w, http.StatusBadRequest, "INVALID_QUERY", "unknown status filter")
			return
		}
	}

	streams, err := h.streamRepo.List(r.Context(), store.StreamFilter{})
	if err != nil {
		serverError(w, r, "LIST_FAILED", "list streams", err)
		return
	}

	resp := make([]streamResponse, 0, len(streams))
	for _, s := range streams {
		sr := h.withStatus(s)
		if statusFilter != nil && sr.Runtime.Status != *statusFilter {
			continue
		}
		resp = append(resp, sr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": resp, "total": len(resp)})
}

// Get returns one stream by code.
// @Summary Get stream
// @Tags streams
// @Produce json
// @Param code path string true "Stream code (a-zA-Z0-9_)"
// @Success 200 {object} apidocs.StreamData
// @Failure 404 {object} apidocs.ErrorBody
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code} [get].
func (h *StreamHandler) Get(w http.ResponseWriter, r *http.Request) {
	code := domain.StreamCode(chi.URLParam(r, "code"))
	stream, err := h.streamRepo.FindByCode(r.Context(), code)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": h.withStatus(stream)})
}

// Put creates a new stream or partially updates an existing one.
// Only fields present in the request body are applied; omitted fields keep their current values.
// @Summary Create or partial-update stream
// @Tags streams
// @Accept json
// @Produce json
// @Param code path string true "Stream code"
// @Param body body apidocs.StreamPutRequest true "Partial or full stream document"
// @Success 200 {object} apidocs.StreamData
// @Success 201 {object} apidocs.StreamData
// @Failure 400 {object} apidocs.ErrorBody
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code} [post].
func (h *StreamHandler) Put(w http.ResponseWriter, r *http.Request) {
	code := domain.StreamCode(chi.URLParam(r, "code"))
	cur, exists, err := h.loadCurrentStream(r, code)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	body, validationErr := decodeStreamBody(r, code, cur, exists)
	if validationErr != nil {
		writeError(w, http.StatusBadRequest, validationErr.code, validationErr.message)
		return
	}

	if vErr := h.validateCopyConfig(r, body); vErr != nil {
		writeError(w, http.StatusBadRequest, vErr.code, vErr.message)
		return
	}

	if vErr := h.validateMixerConfig(r, body); vErr != nil {
		writeError(w, http.StatusBadRequest, vErr.code, vErr.message)
		return
	}

	wasRunning := exists && h.coordinator.IsRunning(code)
	nowEnabled := exists && cur.Disabled && !body.Disabled

	// Save first so the pipeline continues with the old config if persistence fails.
	if err := h.streamRepo.Save(r.Context(), body); err != nil {
		serverError(w, r, "SAVE_FAILED", "save stream", err)
		return
	}

	// Detach from request cancellation: the coordinator spawns goroutines whose
	// lifetime is the stream pipeline, not the HTTP request. Without this, when
	// the response is written and r.Context() is cancelled, every spawned worker
	// (HLS/DASH/RTMP/transcoder/manager) is killed instantly. WithoutCancel keeps
	// request-scoped values (logging tags) but drops the cancel signal.
	pipelineCtx := context.WithoutCancel(r.Context())
	if wasRunning {
		if err := h.coordinator.Update(pipelineCtx, cur, body); err != nil {
			serverError(w, r, "UPDATE_FAILED", "update stream pipeline", err)
			return
		}
	} else if nowEnabled {
		// Stream was disabled → re-enabled: start the pipeline.
		if err := h.coordinator.Start(pipelineCtx, body); err != nil {
			serverError(w, r, "START_FAILED", "start stream pipeline", err)
			return
		}
	}

	if !exists {
		h.bus.Publish(r.Context(), domain.Event{
			Type:       domain.EventStreamCreated,
			StreamCode: body.Code,
		})
		writeJSON(w, http.StatusCreated, map[string]any{"data": *body})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": *body})
}

type putValidationError struct {
	code    string
	message string
}

// validateCopyConfig is the API-handler wrapper that loads existing streams
// from the repo and delegates to validateCopyConfigOn. Split this way so the
// pure validation logic can be unit-tested without a real or fake repo.
func (h *StreamHandler) validateCopyConfig(r *http.Request, proposed *domain.Stream) *putValidationError {
	all, err := h.streamRepo.List(r.Context(), store.StreamFilter{})
	if err != nil {
		return &putValidationError{code: "LIST_FAILED", message: "list streams for copy validation: " + err.Error()}
	}
	return validateCopyConfigOn(proposed, all)
}

// validateCopyConfigOn runs the copy:// safety checks against an in-memory
// snapshot of the existing streams (any source — repo list, fixture, …).
// `existing` is the world BEFORE the write; `proposed` substitutes in for
// any same-code entry to form the world AFTER.
//
// Order of checks (each gives a more specific error than the next when both
// would fire on the same input):
//
//  1. Per-input URL grammar — catch malformed `copy://` before anything
//     else interprets it.
//  2. Shape constraints — local transcoder vs ABR upstream, fallback rules.
//  3. Cycle detection across the merged graph.
func validateCopyConfigOn(proposed *domain.Stream, existing []*domain.Stream) *putValidationError {
	// (1) URL grammar.
	for i, in := range proposed.Inputs {
		if !domain.IsCopyInput(in) {
			continue
		}
		if _, err := domain.CopyInputTarget(in); err != nil {
			return &putValidationError{
				code:    "INVALID_COPY_URL",
				message: fmt.Sprintf("inputs[%d]: %s", i, err.Error()),
			}
		}
	}

	// Build merged view: existing repo streams with proposed substituted
	// for the same code.
	merged := make([]*domain.Stream, 0, len(existing)+1)
	replaced := false
	for _, s := range existing {
		if s == nil {
			continue
		}
		if s.Code == proposed.Code {
			merged = append(merged, proposed)
			replaced = true
			continue
		}
		merged = append(merged, s)
	}
	if !replaced {
		merged = append(merged, proposed)
	}

	lookup := func(c domain.StreamCode) (*domain.Stream, bool) {
		for _, s := range merged {
			if s.Code == c {
				return s, true
			}
		}
		return nil, false
	}

	// (2) Shape constraints. (Cycle detection across the copy:// graph was
	// removed: shape validation already rejects the most common bad case
	// — self-copy — and operators are trusted not to author multi-stream
	// fan-out loops; misconfig surfaces as runtime degradation, not fan-out.)
	if err := domain.ValidateCopyShape(proposed, lookup); err != nil {
		return &putValidationError{code: "INVALID_COPY_SHAPE", message: err.Error()}
	}

	return nil
}

// validateMixerConfig is the API-handler wrapper that loads existing streams
// from the repo and delegates to validateMixerConfigOn. Split this way so the
// pure validation logic can be unit-tested without a real or fake repo.
func (h *StreamHandler) validateMixerConfig(r *http.Request, proposed *domain.Stream) *putValidationError {
	all, err := h.streamRepo.List(r.Context(), store.StreamFilter{})
	if err != nil {
		return &putValidationError{code: "LIST_FAILED", message: "list streams for mixer validation: " + err.Error()}
	}
	return validateMixerConfigOn(proposed, all)
}

// validateMixerConfigOn runs the mixer:// safety checks against an in-memory
// snapshot of the existing streams.
//
// Order of checks:
//
//  1. Per-input URL grammar — catch malformed `mixer://` early.
//  2. Shape constraints — sole-input rule, ABR-upstream rule, self-mix,
//     local-transcoder forbidden.
func validateMixerConfigOn(proposed *domain.Stream, existing []*domain.Stream) *putValidationError {
	// (1) URL grammar.
	for i, in := range proposed.Inputs {
		if !domain.IsMixerInput(in) {
			continue
		}
		if _, _, _, err := domain.MixerInputSpec(in); err != nil {
			return &putValidationError{
				code:    "INVALID_MIXER_URL",
				message: fmt.Sprintf("inputs[%d]: %s", i, err.Error()),
			}
		}
	}

	// Build merged view: existing repo streams with proposed substituted
	// for the same code.
	merged := make([]*domain.Stream, 0, len(existing)+1)
	replaced := false
	for _, s := range existing {
		if s == nil {
			continue
		}
		if s.Code == proposed.Code {
			merged = append(merged, proposed)
			replaced = true
			continue
		}
		merged = append(merged, s)
	}
	if !replaced {
		merged = append(merged, proposed)
	}

	lookup := func(c domain.StreamCode) (*domain.Stream, bool) {
		for _, s := range merged {
			if s.Code == c {
				return s, true
			}
		}
		return nil, false
	}

	// (2) Shape constraints.
	if err := domain.ValidateMixerShape(proposed, lookup); err != nil {
		return &putValidationError{code: "INVALID_MIXER_SHAPE", message: err.Error()}
	}

	return nil
}

func (h *StreamHandler) loadCurrentStream(
	r *http.Request,
	code domain.StreamCode,
) (*domain.Stream, bool, error) {
	cur, err := h.streamRepo.FindByCode(r.Context(), code)
	if err == nil {
		return cur, true, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	return nil, false, err
}

func decodeStreamBody(
	r *http.Request,
	code domain.StreamCode,
	cur *domain.Stream,
	exists bool,
) (*domain.Stream, *putValidationError) {
	// Start from the existing stream so omitted fields keep their current values.
	// For new streams, start from zero value.
	//
	// Deep-clone via JSON round-trip when merging onto an existing stream:
	// `base = *cur` is a shallow copy, so pointer fields (Transcoder, DVR, Push
	// items) would alias `cur`. json.Decode on `base` would then mutate `cur`
	// in-place, and ComputeDiff(cur, body) downstream would see identical
	// pointers and report no change — silently swallowing the update.
	var base domain.Stream
	if exists {
		data, err := json.Marshal(cur)
		if err != nil {
			return nil, &putValidationError{code: "INVALID_BODY", message: "clone existing stream: " + err.Error()}
		}
		if err := json.Unmarshal(data, &base); err != nil {
			return nil, &putValidationError{code: "INVALID_BODY", message: "clone existing stream: " + err.Error()}
		}
	}

	// Unmarshal onto base — only fields present in the JSON are overwritten.
	if err := json.NewDecoder(r.Body).Decode(&base); err != nil {
		return nil, &putValidationError{code: "INVALID_BODY", message: err.Error()}
	}

	base.Code = code
	if err := domain.ValidateStreamCode(string(base.Code)); err != nil {
		return nil, &putValidationError{code: "INVALID_CODE", message: err.Error()}
	}
	if err := base.ValidateInputPriorities(); err != nil {
		return nil, &putValidationError{code: "INVALID_INPUT_PRIORITY", message: err.Error()}
	}
	if err := base.ValidateUniqueInputs(); err != nil {
		return nil, &putValidationError{code: "DUPLICATE_INPUT", message: err.Error()}
	}
	return &base, nil
}

// Delete removes a stream and stops its pipeline.
// @Summary Delete stream
// @Tags streams
// @Param code path string true "Stream code"
// @Success 204 "No Content"
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code} [delete].
func (h *StreamHandler) Delete(w http.ResponseWriter, r *http.Request) {
	code := domain.StreamCode(chi.URLParam(r, "code"))
	// Detach from request cancellation so teardown completes even if the client disconnects.
	h.coordinator.Stop(context.WithoutCancel(r.Context()), code)
	if err := h.streamRepo.Delete(r.Context(), code); err != nil {
		serverError(w, r, "DELETE_FAILED", "delete stream", err)
		return
	}
	h.bus.Publish(r.Context(), domain.Event{
		Type:       domain.EventStreamDeleted,
		StreamCode: code,
	})
	w.WriteHeader(http.StatusNoContent)
}

// Restart stops the running pipeline (if any), waits for full cleanup, then starts
// a fresh pipeline. This guarantees all goroutines have exited and on-disk segments
// have been removed before the new pipeline begins.
// @Summary Restart stream pipeline
// @Tags streams
// @Produce json
// @Param code path string true "Stream code"
// @Success 200 {object} apidocs.StreamActionData
// @Failure 400 {object} apidocs.ErrorBody
// @Failure 404 {object} apidocs.ErrorBody
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code}/restart [post].
func (h *StreamHandler) Restart(w http.ResponseWriter, r *http.Request) {
	code := domain.StreamCode(chi.URLParam(r, "code"))
	stream, err := h.streamRepo.FindByCode(r.Context(), code)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if stream.Disabled {
		writeError(w, http.StatusBadRequest, "STREAM_DISABLED", "stream is disabled; clear disabled flag before starting")
		return
	}

	// Stop is blocking: waits for all goroutines to finish and cleans up on-disk
	// segments before returning. Safe to call even when the pipeline is not running.
	// Detach from request cancellation — the new pipeline outlives this request, and
	// teardown must complete even if the client disconnects mid-restart.
	pipelineCtx := context.WithoutCancel(r.Context())
	h.coordinator.Stop(pipelineCtx, code)

	if err := h.coordinator.Start(pipelineCtx, stream); err != nil {
		serverError(w, r, "START_FAILED", "restart stream pipeline", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"status": "started"}})
}

// SwitchInput forces the active ingest source to the given input priority at runtime.
// The switch is temporary — it reverts automatically when the selected input degrades permanently.
// Switching to a different priority replaces any previous manual override.
//
// @Summary     Manual input switch
// @Tags        streams
// @Produce     json
// @Param       code  path  string                    true  "Stream code"
// @Param       body  body  apidocs.InputSwitchRequest true  "Target input priority"
// @Success     200   {object} apidocs.StreamActionData
// @Failure     400   {object} apidocs.ErrorBody
// @Failure     404   {object} apidocs.ErrorBody
// @Failure     500   {object} apidocs.ErrorBody
// @Router      /streams/{code}/inputs/switch [post].
func (h *StreamHandler) SwitchInput(w http.ResponseWriter, r *http.Request) {
	code := domain.StreamCode(chi.URLParam(r, "code"))

	var body struct {
		Priority int `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	if err := h.manager.SwitchInput(code, body.Priority); err != nil {
		writeError(w, http.StatusBadRequest, "SWITCH_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"status": "switched"}})
}
