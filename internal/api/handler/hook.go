package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/hooks"
	"github.com/open-streamer/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// HookHandler handles webhook/hook management REST endpoints.
type HookHandler struct {
	hookRepo store.HookRepository
	hooks    *hooks.Service
}

// NewHookHandler creates a HookHandler and registers it with the DI injector.
func NewHookHandler(i do.Injector) (*HookHandler, error) {
	return &HookHandler{
		hookRepo: do.MustInvoke[store.HookRepository](i),
		hooks:    do.MustInvoke[*hooks.Service](i),
	}, nil
}

// List registered hooks.
// @Summary List hooks
// @Tags hooks
// @Produce json
// @Success 200 {object} apidocs.HookList
// @Failure 500 {object} apidocs.ErrorBody
// @Router /hooks [get]
func (h *HookHandler) List(w http.ResponseWriter, r *http.Request) {
	hooksList, err := h.hookRepo.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list hooks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": hooksList, "total": len(hooksList)})
}

// Create registers a new hook.
// @Summary Create hook
// @Tags hooks
// @Accept json
// @Produce json
// @Param body body domain.Hook true "Hook configuration"
// @Success 201 {object} apidocs.HookData
// @Failure 400 {object} apidocs.ErrorBody
// @Failure 500 {object} apidocs.ErrorBody
// @Router /hooks [post]
func (h *HookHandler) Create(w http.ResponseWriter, r *http.Request) {
	var hook domain.Hook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := h.hookRepo.Save(r.Context(), &hook); err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "failed to save hook")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": hook})
}

// Get returns one hook.
// @Summary Get hook
// @Tags hooks
// @Produce json
// @Param hid path string true "Hook ID"
// @Success 200 {object} apidocs.HookData
// @Failure 404 {object} apidocs.ErrorBody
// @Router /hooks/{hid} [get]
func (h *HookHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	hook, err := h.hookRepo.FindByID(r.Context(), hid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": hook})
}

// Update replaces hook configuration.
// @Summary Update hook
// @Tags hooks
// @Accept json
// @Produce json
// @Param hid path string true "Hook ID"
// @Param body body domain.Hook true "Hook configuration"
// @Success 200 {object} apidocs.HookData
// @Failure 400 {object} apidocs.ErrorBody
// @Failure 404 {object} apidocs.ErrorBody
// @Failure 500 {object} apidocs.ErrorBody
// @Router /hooks/{hid} [put]
func (h *HookHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	if _, err := h.hookRepo.FindByID(r.Context(), hid); err != nil {
		writeStoreError(w, err)
		return
	}

	var hook domain.Hook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	hook.ID = hid
	if err := h.hookRepo.Save(r.Context(), &hook); err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "failed to save hook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": hook})
}

// Delete removes a hook.
// @Summary Delete hook
// @Tags hooks
// @Param hid path string true "Hook ID"
// @Success 204 "No Content"
// @Failure 500 {object} apidocs.ErrorBody
// @Router /hooks/{hid} [delete]
func (h *HookHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	if err := h.hookRepo.Delete(r.Context(), hid); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete hook")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Test delivers a synthetic event to an HTTP hook (same signing/delivery path as production).
// @Summary Test hook delivery
// @Tags hooks
// @Produce json
// @Param hid path string true "Hook ID"
// @Success 200 {object} apidocs.HookTestData
// @Failure 400 {object} apidocs.ErrorBody
// @Failure 404 {object} apidocs.ErrorBody
// @Failure 502 {object} apidocs.ErrorBody
// @Router /hooks/{hid}/test [post]
func (h *HookHandler) Test(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	if err := h.hooks.DeliverTestEvent(r.Context(), hid); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeStoreError(w, err)
		case errors.Is(err, hooks.ErrHookTestUnsupported):
			writeError(w, http.StatusBadRequest, "TEST_UNSUPPORTED", err.Error())
		default:
			writeError(w, http.StatusBadGateway, "DELIVERY_FAILED", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]string{
			"status": "delivered",
			"at":     time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
}
