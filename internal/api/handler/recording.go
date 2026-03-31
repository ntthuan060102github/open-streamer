package handler

import (
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/dvr"
	"github.com/open-streamer/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// RecordingHandler handles DVR recording and playback REST endpoints.
type RecordingHandler struct {
	dvr        *dvr.Service
	recRepo    store.RecordingRepository
	streamRepo store.StreamRepository
}

// NewRecordingHandler creates a RecordingHandler and registers it with the DI injector.
func NewRecordingHandler(i do.Injector) (*RecordingHandler, error) {
	return &RecordingHandler{
		dvr:        do.MustInvoke[*dvr.Service](i),
		recRepo:    do.MustInvoke[store.RecordingRepository](i),
		streamRepo: do.MustInvoke[store.StreamRepository](i),
	}, nil
}

// Start begins DVR recording for the stream.
// @Summary Start recording
// @Tags recordings
// @Produce json
// @Param code path string true "Stream code"
// @Success 201 {object} apidocs.RecordingData
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code}/recordings/start [post]
func (h *RecordingHandler) Start(w http.ResponseWriter, r *http.Request) {
	streamCode := domain.StreamCode(chi.URLParam(r, "code"))

	var segmentDuration time.Duration
	if stream, err := h.streamRepo.FindByCode(r.Context(), streamCode); err == nil && stream != nil && stream.DVR != nil {
		segmentDuration = time.Duration(stream.DVR.SegmentDuration) * time.Second
	}

	rec, err := h.dvr.StartRecording(r.Context(), streamCode, segmentDuration)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RECORDING_START_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": rec})
}

// Stop ends the active recording for the stream.
// @Summary Stop recording
// @Tags recordings
// @Produce json
// @Param code path string true "Stream code"
// @Success 200 {object} apidocs.StreamActionData
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code}/recordings/stop [post]
func (h *RecordingHandler) Stop(w http.ResponseWriter, r *http.Request) {
	streamCode := domain.StreamCode(chi.URLParam(r, "code"))
	if err := h.dvr.StopRecording(r.Context(), streamCode); err != nil {
		writeError(w, http.StatusInternalServerError, "RECORDING_STOP_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"status": "stopped"}})
}

// ListByStream lists recording metadata for a stream.
// @Summary List recordings by stream
// @Tags recordings
// @Produce json
// @Param code path string true "Stream code"
// @Success 200 {object} apidocs.RecordingList
// @Failure 500 {object} apidocs.ErrorBody
// @Router /streams/{code}/recordings [get]
func (h *RecordingHandler) ListByStream(w http.ResponseWriter, r *http.Request) {
	streamCode := domain.StreamCode(chi.URLParam(r, "code"))
	recs, err := h.recRepo.ListByStream(r.Context(), streamCode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list recordings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": recs, "total": len(recs)})
}

// Get returns one recording by id.
// @Summary Get recording
// @Tags recordings
// @Produce json
// @Param rid path string true "Recording ID"
// @Success 200 {object} apidocs.RecordingData
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid} [get]
func (h *RecordingHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rec})
}

// Delete removes recording metadata.
// @Summary Delete recording
// @Tags recordings
// @Param rid path string true "Recording ID"
// @Success 204 "No Content"
// @Failure 500 {object} apidocs.ErrorBody
// @Router /recordings/{rid} [delete]
func (h *RecordingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	if err := h.recRepo.Delete(r.Context(), rid); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete recording")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Playlist serves a VOD HLS playlist (M3U8) for the recording.
// @Summary Recording VOD playlist
// @Tags recordings
// @Produce plain
// @Param rid path string true "Recording ID"
// @Success 200 {string} string "M3U8 body"
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid}/playlist.m3u8 [get]
func (h *RecordingHandler) Playlist(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(rec.Segments) == 0 {
		writeError(w, http.StatusNotFound, "NO_SEGMENTS", "recording has no segments yet")
		return
	}

	maxSec := 1.0
	for _, seg := range rec.Segments {
		if d := seg.Duration.Seconds(); d > maxSec {
			maxSec = d
		}
	}
	target := int(math.Ceil(maxSec))
	if target < 1 {
		target = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	for _, seg := range rec.Segments {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", seg.Duration.Seconds())
		name := filepath.Base(seg.Path)
		if name == "." || name == "/" {
			name = fmt.Sprintf("%d.ts", seg.Index)
		}
		b.WriteString(name)
		b.WriteByte('\n')
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
