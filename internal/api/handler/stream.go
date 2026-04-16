// Package handler contains HTTP request handlers for the API server.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ntt0601zcoder/open-streamer/internal/coordinator"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/manager"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// StreamHandler handles stream lifecycle REST endpoints.
type StreamHandler struct {
	streamRepo  store.StreamRepository
	coordinator *coordinator.Coordinator
	manager     *manager.Service
	bus         events.Bus
}

// streamResponse is the API representation of a stream.
// It embeds the persisted domain.Stream (whose Status field is json:"-") and
// overlays runtime-computed fields so clients always see the live state.
type streamResponse struct {
	*domain.Stream
	Status         domain.StreamStatus    `json:"status"`
	PipelineActive bool                   `json:"pipeline_active"`
	Runtime        *manager.RuntimeStatus `json:"runtime,omitempty"`
}

func (h *StreamHandler) withStatus(s *domain.Stream) streamResponse {
	resp := streamResponse{
		Stream: s,
		Status: h.coordinator.StreamStatus(s.Code),
	}
	if rt, ok := h.manager.RuntimeStatus(s.Code); ok {
		resp.PipelineActive = true
		resp.Runtime = &rt
	}
	return resp
}

// NewStreamHandler creates a StreamHandler and registers it with the DI injector.
func NewStreamHandler(i do.Injector) (*StreamHandler, error) {
	return &StreamHandler{
		streamRepo:  do.MustInvoke[store.StreamRepository](i),
		coordinator: do.MustInvoke[*coordinator.Coordinator](i),
		manager:     do.MustInvoke[*manager.Service](i),
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
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list streams")
		return
	}

	resp := make([]streamResponse, 0, len(streams))
	for _, s := range streams {
		sr := h.withStatus(s)
		if statusFilter != nil && sr.Status != *statusFilter {
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
		writeStoreError(w, err)
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
		writeStoreError(w, err)
		return
	}

	body, validationErr := decodeStreamBody(r, code, cur, exists)
	if validationErr != nil {
		writeError(w, http.StatusBadRequest, validationErr.code, validationErr.message)
		return
	}

	wasRunning := exists && h.coordinator.IsRunning(code)
	nowEnabled := exists && cur.Disabled && !body.Disabled

	// Save first so the pipeline continues with the old config if persistence fails.
	if err := h.streamRepo.Save(r.Context(), body); err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "failed to save stream")
		return
	}

	if wasRunning {
		if err := h.coordinator.Update(r.Context(), cur, body); err != nil {
			writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
			return
		}
	} else if nowEnabled {
		// Stream was disabled → re-enabled: start the pipeline.
		if err := h.coordinator.Start(r.Context(), body); err != nil {
			writeError(w, http.StatusInternalServerError, "START_FAILED", err.Error())
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
	var base domain.Stream
	if exists {
		base = *cur
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

	if exists {
		base.CreatedAt = cur.CreatedAt
	} else {
		base.CreatedAt = time.Now()
	}
	base.UpdatedAt = time.Now()
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
	h.coordinator.Stop(code)
	if err := h.streamRepo.Delete(r.Context(), code); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete stream")
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
		writeStoreError(w, err)
		return
	}
	if stream.Disabled {
		writeError(w, http.StatusBadRequest, "STREAM_DISABLED", "stream is disabled; clear disabled flag before starting")
		return
	}

	// Stop is blocking: waits for all goroutines to finish and cleans up on-disk
	// segments before returning. Safe to call even when the pipeline is not running.
	h.coordinator.Stop(code)

	if err := h.coordinator.Start(r.Context(), stream); err != nil {
		writeError(w, http.StatusInternalServerError, "START_FAILED", err.Error())
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
