package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/hooks"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// HookHandler handles webhook and hook management REST endpoints.
type HookHandler struct {
	hookRepo store.HookRepository
	hooks    *hooks.Service
}

// hookResponse mirrors domain.Hook with the resolved-defaults view in
// `effective`. Per-hook MaxRetries / TimeoutSec fall back to server-wide
// defaults when 0 — UI uses Effective to render those concrete numbers.
type hookResponse struct {
	*domain.Hook
	Effective *domain.Hook `json:"effective,omitempty"`
}

func newHookResponse(h *domain.Hook) hookResponse {
	return hookResponse{Hook: h, Effective: domain.EffectiveHook(h)}
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
// @Router /hooks [get].
func (h *HookHandler) List(w http.ResponseWriter, r *http.Request) {
	hooksList, err := h.hookRepo.List(r.Context())
	if err != nil {
		serverError(w, r, "LIST_FAILED", "list hooks", err)
		return
	}
	resp := make([]hookResponse, 0, len(hooksList))
	for _, hk := range hooksList {
		resp = append(resp, newHookResponse(hk))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": resp, "total": len(resp)})
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
// @Router /hooks [post].
func (h *HookHandler) Create(w http.ResponseWriter, r *http.Request) {
	var hook domain.Hook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := h.hookRepo.Save(r.Context(), &hook); err != nil {
		serverError(w, r, "SAVE_FAILED", "create hook", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": newHookResponse(&hook)})
}

// Get returns one hook.
// @Summary Get hook
// @Tags hooks
// @Produce json
// @Param hid path string true "Hook ID"
// @Success 200 {object} apidocs.HookData
// @Failure 404 {object} apidocs.ErrorBody
// @Router /hooks/{hid} [get].
func (h *HookHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	hook, err := h.hookRepo.FindByID(r.Context(), hid)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": newHookResponse(hook)})
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
// @Router /hooks/{hid} [put].
func (h *HookHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	if _, err := h.hookRepo.FindByID(r.Context(), hid); err != nil {
		writeStoreError(w, r, err)
		return
	}

	var hook domain.Hook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	hook.ID = hid
	if err := h.hookRepo.Save(r.Context(), &hook); err != nil {
		serverError(w, r, "SAVE_FAILED", "update hook", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": newHookResponse(&hook)})
}

// Delete removes a hook.
// @Summary Delete hook
// @Tags hooks
// @Param hid path string true "Hook ID"
// @Success 204 "No Content"
// @Failure 500 {object} apidocs.ErrorBody
// @Router /hooks/{hid} [delete].
func (h *HookHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	if err := h.hookRepo.Delete(r.Context(), hid); err != nil {
		serverError(w, r, "DELETE_FAILED", "delete hook", err)
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
// @Router /hooks/{hid}/test [post].
func (h *HookHandler) Test(w http.ResponseWriter, r *http.Request) {
	hid := domain.HookID(chi.URLParam(r, "hid"))
	if err := h.hooks.DeliverTestEvent(r.Context(), hid); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeStoreError(w, r, err)
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
