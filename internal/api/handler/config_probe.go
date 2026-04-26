package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/hwdetect"
	"github.com/ntt0601zcoder/open-streamer/internal/transcoder"
)

// probeRequest is the body shape for POST /config/transcoder/probe.
//
// FFmpegPath empty → server probes "ffmpeg" via $PATH (matches the
// runtime default at publisher.NewService and transcoder.Service).
//
// The set of encoders to inspect is auto-derived from the HOST'S
// available hardware (hwdetect.Available()) — caller does not pick
// which HW to probe for. Server with NVIDIA card → response includes
// NVENC encoders; CPU-only host → response excludes them.
type probeRequest struct {
	FFmpegPath string `json:"ffmpeg_path"`
}

// probeRequestTimeout caps the entire probe call (ffmpeg sub-invocations
// each have their own internal timeout, but this guards against the
// handler hanging if the OS process table itself wedges).
const probeRequestTimeout = 15 * time.Second

// ProbeTranscoder runs a capability probe against the FFmpeg binary at
// the requested path WITHOUT touching the live config. UI calls this
// behind a "Test" button so operators can validate a candidate path
// before committing it via POST /config.
//
// Response: 200 with the full ProbeResult regardless of compatibility —
// the result's `ok` field tells the caller whether the binary passes.
// 4xx is reserved for malformed bodies; 5xx for invocation errors
// (binary missing, not executable, not actually FFmpeg).
//
// @Summary     Probe an FFmpeg binary for app compatibility.
// @Description Inspects the binary at ffmpeg_path (empty = $PATH) and reports which required / optional encoders + muxers are available. Pure check — does not modify config.
// @Tags        system
// @Accept      json
// @Produce     json
// @Param       body body handler.probeRequest true "Path to probe (empty = use $PATH)"
// @Success     200  {object} transcoder.ProbeResult
// @Failure     400  {object} apidocs.ErrorBody
// @Failure     502  {object} apidocs.ErrorBody
// @Router      /config/transcoder/probe [post].
func (h *ConfigHandler) ProbeTranscoder(w http.ResponseWriter, r *http.Request) {
	var body probeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeRequestTimeout)
	defer cancel()

	// Server auto-detects which HW backends the host actually exposes;
	// the response's optional encoder set covers exactly those (plus
	// audio + CPU). Frontend doesn't need to know what's installed —
	// it just renders the list it gets back.
	res, err := transcoder.Probe(ctx, body.FFmpegPath, hwdetect.Available())
	if err != nil {
		// Binary unusable (not found / not executable / not FFmpeg).
		// 502 — the path the caller supplied points to something the
		// server cannot speak to. Body still carries the path for UI.
		writeError(w, http.StatusBadGateway, "FFMPEG_UNUSABLE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// transcoderPathChanged reports whether the FFmpegPath field is
// different between current and proposed configs. Probing is the
// expensive part of save validation, so callers gate on this — unrelated
// edits (buffer.capacity, hooks.worker_count, …) skip the probe.
func (h *ConfigHandler) transcoderPathChanged(current, proposed *domain.GlobalConfig) bool {
	oldPath, newPath := "", ""
	if current != nil && current.Transcoder != nil {
		oldPath = strings.TrimSpace(current.Transcoder.FFmpegPath)
	}
	if proposed != nil && proposed.Transcoder != nil {
		newPath = strings.TrimSpace(proposed.Transcoder.FFmpegPath)
	}
	return oldPath != newPath
}

// validateTranscoderPath probes the proposed FFmpeg binary and returns
// a putValidationError when REQUIRED capabilities are missing or the
// binary is unusable. Returns nil when the probe passes (warnings about
// optional capabilities are not blocking — operator may intentionally
// run a stripped build).
func (h *ConfigHandler) validateTranscoderPath(ctx context.Context, proposed *domain.GlobalConfig) *putValidationError {
	probeCtx, cancel := context.WithTimeout(ctx, probeRequestTimeout)
	defer cancel()

	path := ""
	if proposed != nil && proposed.Transcoder != nil {
		path = proposed.Transcoder.FFmpegPath
	}
	// Use host-detected backends — same scope as the probe endpoint.
	// REQUIRED set is constant (libx264 + aac + mpegts) so save
	// validation only really cares whether those three are present;
	// optional warnings are informational.
	res, err := transcoder.Probe(probeCtx, path, hwdetect.Available())
	if err != nil {
		return &putValidationError{
			code:    "FFMPEG_UNUSABLE",
			message: "ffmpeg binary cannot be invoked: " + err.Error(),
		}
	}
	if !res.OK {
		return &putValidationError{
			code:    "FFMPEG_INCOMPATIBLE",
			message: "ffmpeg lacks required capabilities: " + strings.Join(res.Errors, "; "),
		}
	}
	return nil
}
