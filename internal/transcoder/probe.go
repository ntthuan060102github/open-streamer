package transcoder

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// ProbeResult is the structured outcome of inspecting an FFmpeg binary
// for compatibility with this app. Returned by Probe.
//
// OK is true when no REQUIRED capability is missing (server can boot /
// serve any stream that uses default settings). Warnings list OPTIONAL
// capabilities that are absent — the server still runs but specific
// configurations (HW acceleration, h265, vp9, av1) will fail at runtime
// for streams that select them.
type ProbeResult struct {
	OK       bool                       `json:"ok"`
	Path     string                     `json:"path"`
	Version  string                     `json:"version,omitempty"`
	Encoders map[string]map[string]bool `json:"encoders"`
	Muxers   map[string]bool            `json:"muxers"`
	Warnings []string                   `json:"warnings,omitempty"`
	Errors   []string                   `json:"errors,omitempty"`
}

// Required capability sets — missing any of these means OK=false.
//
// libx264: CPU h264 fallback when HW=none. Always reachable from any
// stream config (default codec).
// aac: audio default; Audio.Copy=false + empty codec → aac.
// mpegts: server uses MPEG-TS as the wire format between ingestor →
// transcoder → publisher. Without it nothing works.
var (
	requiredEncoders = []string{"libx264", "aac"}
	requiredMuxers   = []string{"mpegts"}
)

// Optional capability sets — missing means specific configs will fail
// but the server still boots. UI surfaces these as warnings + can
// disable the corresponding selections in dropdowns.
var optionalMuxers = []string{"hls", "dash"}

// hwOptionalEncoders maps a hardware backend to the video encoders that
// are relevant FOR THAT BACKEND. Used to filter probe output so the UI
// shows only encoders applicable to the operator's current HW selection.
//
// HWAccelNone covers the CPU pipeline — the alternative video codecs
// (h265, vp9, av1) are reachable only without HW acceleration.
//
// Each HW backend that supports an explicit encoder name (e.g. h264_qsv)
// lists it here so operators picking that backend get a flat "this is
// available / missing" view of what they care about.
var hwOptionalEncoders = map[domain.HWAccel][]string{
	domain.HWAccelNone: {
		"libx265",    // h265 CPU
		"libvpx-vp9", // vp9
		"libsvtav1",  // av1
	},
	domain.HWAccelNVENC: {
		"h264_nvenc",
		"hevc_nvenc",
	},
	domain.HWAccelVAAPI: {
		"h264_vaapi",
		"hevc_vaapi",
	},
	domain.HWAccelQSV: {
		"h264_qsv",
		"hevc_qsv",
	},
	domain.HWAccelVideoToolbox: {
		"h264_videotoolbox",
		"hevc_videotoolbox",
	},
}

// audioOptionalEncoders are HW-independent — included regardless of the
// caller's hw filter (audio doesn't have HW variants in this app).
var audioOptionalEncoders = []string{
	"libopus",
	"libmp3lame",
	"ac3",
}

// optionalEncodersForBackends returns the union of optional encoders
// across the given HW backends, plus the HW-independent audio set.
//
// Caller is expected to pass the set of backends actually present on
// the host (typically hwdetect.Available()). Empty slice → full union
// across every known backend (used as a defensive fallback).
//
// Order is preserved across the input slice so the response stays
// deterministic — frontend caching depends on byte-stable output.
func optionalEncodersForBackends(hws []domain.HWAccel) []string {
	if len(hws) == 0 {
		hws = []domain.HWAccel{
			domain.HWAccelNone,
			domain.HWAccelNVENC,
			domain.HWAccelVAAPI,
			domain.HWAccelQSV,
			domain.HWAccelVideoToolbox,
		}
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(audioOptionalEncoders)+len(hws)*2)
	for _, hw := range hws {
		for _, name := range hwOptionalEncoders[hw] {
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	for _, name := range audioOptionalEncoders {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// probeTimeout caps each ffmpeg sub-invocation so a hung binary doesn't
// block boot or an HTTP request indefinitely.
const probeTimeout = 5 * time.Second

// versionRE captures the version token from the first line of
// `ffmpeg -version`, e.g. "ffmpeg version 6.1.1-0ubuntu1 …" → "6.1.1".
// Tolerates suffixes (-0ubuntu1, -tessus, n6.0, etc.) by stopping at the
// first whitespace after the literal "version".
var versionRE = regexp.MustCompile(`(?i)^ffmpeg version\s+(\S+)`)

// Probe runs the FFmpeg binary at path and reports compatibility with
// this app. Returns an error only when the binary itself cannot be
// invoked (not found, not executable, not actually FFmpeg). For "ran but
// missing capabilities", Probe returns a non-nil ProbeResult with
// OK=false and Errors populated.
//
// Empty path is normalised to "ffmpeg" (PATH lookup) — matches the
// runtime default at publisher.NewService and transcoder.Service.
//
// hws filters the OPTIONAL encoder set to encoders relevant to the
// host's available backends. Caller passes the result of
// hwdetect.Available() — server auto-detects the hardware, the client
// (UI / boot) does NOT pick. Empty slice → union across every backend
// (defensive fallback).
//
// REQUIRED set (libx264, aac, mpegts) is constant — independent of hw.
func Probe(ctx context.Context, path string, hws []domain.HWAccel) (*ProbeResult, error) {
	if strings.TrimSpace(path) == "" {
		path = "ffmpeg"
	}

	optionalEncoders := optionalEncodersForBackends(hws)

	res := &ProbeResult{
		Path:     path,
		Encoders: make(map[string]map[string]bool, 2),
		Muxers:   make(map[string]bool, len(requiredMuxers)+len(optionalMuxers)),
	}
	res.Encoders["required"] = make(map[string]bool, len(requiredEncoders))
	res.Encoders["optional"] = make(map[string]bool, len(optionalEncoders))

	verOut, err := runProbe(ctx, path, "-version")
	if err != nil {
		// Binary itself is unusable — surface as error so caller can
		// distinguish "missing binary" from "runs but lacks features".
		return nil, fmt.Errorf("invoke %q: %w", path, err)
	}
	if !strings.Contains(verOut, "ffmpeg version") {
		return nil, fmt.Errorf("%q is not an FFmpeg binary (no 'ffmpeg version' banner)", path)
	}
	res.Version = parseFFmpegVersion(verOut)

	encOut, err := runProbe(ctx, path, "-hide_banner", "-encoders")
	if err != nil {
		return nil, fmt.Errorf("list encoders: %w", err)
	}
	muxOut, err := runProbe(ctx, path, "-hide_banner", "-muxers")
	if err != nil {
		return nil, fmt.Errorf("list muxers: %w", err)
	}

	for _, name := range requiredEncoders {
		res.Encoders["required"][name] = encoderPresent(encOut, name)
	}
	for _, name := range optionalEncoders {
		res.Encoders["optional"][name] = encoderPresent(encOut, name)
	}
	for _, name := range requiredMuxers {
		res.Muxers[name] = muxerPresent(muxOut, name)
	}
	for _, name := range optionalMuxers {
		res.Muxers[name] = muxerPresent(muxOut, name)
	}

	for _, name := range requiredEncoders {
		if !res.Encoders["required"][name] {
			res.Errors = append(res.Errors,
				fmt.Sprintf("REQUIRED encoder %q missing — server cannot transcode default streams", name))
		}
	}
	for _, name := range requiredMuxers {
		if !res.Muxers[name] {
			res.Errors = append(res.Errors,
				fmt.Sprintf("REQUIRED muxer/demuxer %q missing — server wire format depends on it", name))
		}
	}
	for _, name := range optionalEncoders {
		if !res.Encoders["optional"][name] {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("optional encoder %q missing — streams selecting it will fail", name))
		}
	}
	for _, name := range optionalMuxers {
		if !res.Muxers[name] {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("optional muxer %q missing — corresponding output format will fail", name))
		}
	}

	res.OK = len(res.Errors) == 0
	return res, nil
}

// runProbe wraps an ffmpeg sub-invocation with a timeout + combined
// stdout+stderr capture (older ffmpeg writes the version banner to
// stderr; newer to stdout — we accept either).
func runProbe(parent context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Distinguish exec error (binary missing / not executable) from
		// non-zero exit. Both paths return error to caller.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("ffmpeg %s exit %d: %s",
				strings.Join(args, " "), exitErr.ExitCode(), trimOutput(out))
		}
		return "", err
	}
	return string(out), nil
}

// parseFFmpegVersion extracts the version token from `ffmpeg -version`
// output's first line. Returns "" if the format is unrecognised.
func parseFFmpegVersion(out string) string {
	scanner := strings.SplitN(out, "\n", 2)
	if len(scanner) == 0 {
		return ""
	}
	m := versionRE.FindStringSubmatch(scanner[0])
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// encoderPresent reports whether `ffmpeg -encoders` lists the given
// encoder name. The output format is one encoder per line:
//
//	V..... libx264              libx264 H.264 / AVC ...
//	A..... aac                  AAC (Advanced Audio Coding)
//
// We match the encoder NAME (second whitespace-delimited token) exactly,
// so "libx264" doesn't accidentally match "libx264rgb".
func encoderPresent(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// Expect: <flags> <name> <description...>
		if len(fields) < 2 {
			continue
		}
		// First field is flags like "V....D" / "A....." — must contain
		// the type character. Skip header lines that don't match.
		if !strings.ContainsAny(fields[0], "VAS") {
			continue
		}
		if fields[1] == name {
			return true
		}
	}
	return false
}

// muxerPresent reports whether `ffmpeg -muxers` lists the given muxer
// name. Same shape as encoders but flag column is " E" or "DE".
func muxerPresent(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if !strings.ContainsAny(fields[0], "DE") {
			continue
		}
		if fields[1] == name {
			return true
		}
	}
	return false
}

// trimOutput truncates probe output to a sane length for embedding in
// error messages — full ffmpeg dumps can be hundreds of KB.
func trimOutput(b []byte) string {
	const maxLen = 512
	s := strings.TrimSpace(string(b))
	if len(s) > maxLen {
		return s[:maxLen] + "… (truncated)"
	}
	return s
}
