package handler

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/dvr"
	"github.com/ntthuan060102github/open-streamer/internal/store"
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
// @Router /streams/{code}/recordings/start [post].
func (h *RecordingHandler) Start(w http.ResponseWriter, r *http.Request) {
	streamCode := domain.StreamCode(chi.URLParam(r, "code"))

	var dvrCfg *domain.StreamDVRConfig
	var mediaBuf domain.StreamCode
	if stream, err := h.streamRepo.FindByCode(r.Context(), streamCode); err == nil && stream != nil {
		dvrCfg = stream.DVR
		mediaBuf = buffer.PlaybackBufferID(streamCode, stream.Transcoder)
	}

	rec, err := h.dvr.StartRecording(r.Context(), streamCode, mediaBuf, dvrCfg)
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
// @Router /streams/{code}/recordings/stop [post].
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
// @Router /streams/{code}/recordings [get].
func (h *RecordingHandler) ListByStream(w http.ResponseWriter, r *http.Request) {
	streamCode := domain.StreamCode(chi.URLParam(r, "code"))
	recs, err := h.recRepo.ListByStream(r.Context(), streamCode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list recordings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": recs, "total": len(recs)})
}

// Get returns one recording's lifecycle metadata by id.
// @Summary Get recording
// @Tags recordings
// @Produce json
// @Param rid path string true "Recording ID (= stream code)"
// @Success 200 {object} apidocs.RecordingData
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid} [get].
func (h *RecordingHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rec})
}

// Info returns full DVR metadata for a recording: dvr_range, gaps, segment count, total size.
// Data is read from index.json on disk, so it is always up-to-date.
// @Summary DVR recording info
// @Tags recordings
// @Produce json
// @Param rid path string true "Recording ID (= stream code)"
// @Success 200 {object} map[string]any
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid}/info [get].
func (h *RecordingHandler) Info(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if rec.SegmentDir == "" {
		writeError(w, http.StatusNotFound, "NO_DATA", "recording has no data yet")
		return
	}

	idx, err := h.dvr.LoadIndex(rec.SegmentDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INDEX_READ_FAILED", err.Error())
		return
	}
	if idx == nil {
		writeError(w, http.StatusNotFound, "NO_DATA", "recording has no data yet")
		return
	}

	type dvrRange struct {
		StartedAt     time.Time `json:"started_at"`
		LastSegmentAt time.Time `json:"last_segment_at,omitempty"`
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"stream_code": rec.StreamCode,
			"status":      rec.Status,
			"dvr_range": dvrRange{
				StartedAt:     idx.StartedAt,
				LastSegmentAt: idx.LastSegmentAt,
			},
			"gaps":            idx.Gaps,
			"segment_count":   idx.SegmentCount,
			"total_size_bytes": idx.TotalSizeBytes,
		},
	})
}

// Delete removes recording metadata from the store.
// Does NOT delete segment files on disk.
// @Summary Delete recording metadata
// @Tags recordings
// @Param rid path string true "Recording ID (= stream code)"
// @Success 204 "No Content"
// @Failure 500 {object} apidocs.ErrorBody
// @Router /recordings/{rid} [delete].
func (h *RecordingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	if err := h.recRepo.Delete(r.Context(), rid); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete recording")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Playlist serves the M3U8 playlist written to disk by the DVR worker.
// Reads playlist.m3u8 directly for an always-current response.
// @Summary Recording M3U8 playlist
// @Tags recordings
// @Produce plain
// @Param rid path string true "Recording ID (= stream code)"
// @Success 200 {string} string "M3U8 body"
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid}/playlist.m3u8 [get].
func (h *RecordingHandler) Playlist(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if rec.SegmentDir == "" {
		writeError(w, http.StatusNotFound, "NO_SEGMENTS", "recording has no segments yet")
		return
	}

	data, err := os.ReadFile(filepath.Join(rec.SegmentDir, "playlist.m3u8"))
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "NO_PLAYLIST", "playlist not yet available")
			return
		}
		writeError(w, http.StatusInternalServerError, "READ_PLAYLIST_FAILED", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ServeSegment serves a DVR TS segment file from the recording's SegmentDir.
// @Summary Serve DVR segment
// @Tags recordings
// @Produce octet-stream
// @Param rid path string true "Recording ID (= stream code)"
// @Param file path string true "Segment filename (e.g. 000000.ts)"
// @Success 200 {file} binary
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid}/{file} [get].
func (h *RecordingHandler) ServeSegment(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))
	file := filepath.Base(chi.URLParam(r, "file")) // strip path traversal

	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if rec.SegmentDir == "" {
		writeError(w, http.StatusNotFound, "SEGMENT_NOT_FOUND", "segment not found")
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	http.ServeFile(w, r, filepath.Join(rec.SegmentDir, file))
}

// Timeshift builds and returns a dynamic VOD M3U8 for a time window.
//
// Query parameters (mutually exclusive for start point):
//   - from=<RFC3339>   absolute start time, e.g. 2026-04-06T14:30:00Z
//   - offset_sec=<N>   seconds from recording start (relative)
//
// Optional:
//   - duration=<N>     window length in seconds (default: all remaining)
//
// Example:
//
//	GET /recordings/test/timeshift.m3u8?from=2026-04-06T14:30:00Z&duration=3600
//	GET /recordings/test/timeshift.m3u8?offset_sec=1800&duration=3600
//
// @Summary Timeshift VOD playlist
// @Tags recordings
// @Produce plain
// @Param rid       path  string true  "Recording ID (= stream code)"
// @Param from      query string false "Absolute start time (RFC3339)"
// @Param offset_sec query int   false "Relative start offset in seconds from recording start"
// @Param duration  query int   false "Window duration in seconds (default: all remaining)"
// @Success 200 {string} string "M3U8 body"
// @Failure 400 {object} apidocs.ErrorBody
// @Failure 404 {object} apidocs.ErrorBody
// @Router /recordings/{rid}/timeshift.m3u8 [get].
func (h *RecordingHandler) Timeshift(w http.ResponseWriter, r *http.Request) {
	rid := domain.RecordingID(chi.URLParam(r, "rid"))

	rec, err := h.recRepo.FindByID(r.Context(), rid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if rec.SegmentDir == "" {
		writeError(w, http.StatusNotFound, "NO_DATA", "recording has no data yet")
		return
	}

	// Parse all segments from playlist.m3u8.
	segments, err := h.dvr.ParsePlaylist(rec.SegmentDir)
	if err != nil || len(segments) == 0 {
		writeError(w, http.StatusNotFound, "NO_PLAYLIST", "playlist not yet available")
		return
	}

	// Determine start time.
	var startTime time.Time
	if raw := r.URL.Query().Get("from"); raw != "" {
		startTime, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_FROM", "from must be RFC3339, e.g. 2026-04-06T14:30:00Z")
			return
		}
	} else if raw := r.URL.Query().Get("offset_sec"); raw != "" {
		offsetSec, err := strconv.ParseFloat(raw, 64)
		if err != nil || offsetSec < 0 {
			writeError(w, http.StatusBadRequest, "INVALID_OFFSET", "offset_sec must be a non-negative number")
			return
		}
		// Anchor to first segment's wall time + offset.
		startTime = segments[0].WallTime.Add(time.Duration(offsetSec * float64(time.Second)))
	} else {
		// No start → return from beginning (equivalent to full playlist).
		startTime = segments[0].WallTime
	}

	// Determine window duration (0 = unlimited).
	var windowDur time.Duration
	if raw := r.URL.Query().Get("duration"); raw != "" {
		durSec, err := strconv.ParseFloat(raw, 64)
		if err != nil || durSec <= 0 {
			writeError(w, http.StatusBadRequest, "INVALID_DURATION", "duration must be a positive number of seconds")
			return
		}
		windowDur = time.Duration(durSec * float64(time.Second))
	}

	// Slice the segment list to the requested window.
	// A segment is included if its wall_time >= startTime and it falls within windowDur.
	endTime := startTime.Add(windowDur)
	var window []dvr.SegmentMeta
	for _, seg := range segments {
		if seg.WallTime.IsZero() {
			continue
		}
		segEnd := seg.WallTime.Add(seg.Duration)
		// Include if segment overlaps [startTime, endTime).
		if segEnd.Before(startTime) || segEnd.Equal(startTime) {
			continue
		}
		if windowDur > 0 && seg.WallTime.After(endTime) {
			break
		}
		window = append(window, seg)
	}

	if len(window) == 0 {
		writeError(w, http.StatusNotFound, "NO_SEGMENTS_IN_RANGE", "no segments found for the requested time range")
		return
	}

	// Build VOD M3U8.
	maxSec := 1.0
	for _, seg := range window {
		if d := seg.Duration.Seconds(); d > maxSec {
			maxSec = d
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxSec)))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteByte('\n')

	needDateTime := true
	for _, seg := range window {
		if seg.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
			needDateTime = true
		}
		if needDateTime && !seg.WallTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				seg.WallTime.UTC().Format("2006-01-02T15:04:05.000Z"))
			needDateTime = false
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", seg.Duration.Seconds())
		fmt.Fprintf(&b, "%06d.ts\n", seg.Index)
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
