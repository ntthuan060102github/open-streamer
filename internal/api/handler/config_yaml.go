package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
)

const (
	maxYAMLBodyBytes = 4 << 20 // 4 MiB — generous enough for many streams + hooks, blocks accidental huge uploads
	yamlContentType  = "application/yaml"
	msgMustBeGEZero  = "must be >= 0"
)

// fullConfig is the unified schema exposed to the frontend editor.
// It bundles every editable resource so the user can manage the whole system
// from a single YAML document — change a port, add a stream, register a hook,
// all in one round-trip.
type fullConfig struct {
	GlobalConfig *domain.GlobalConfig `yaml:"global_config"`
	Streams      []*domain.Stream     `yaml:"streams"`
	Hooks        []*domain.Hook       `yaml:"hooks"`
	VOD          []*domain.VODMount   `yaml:"vod"`
}

// fieldError describes a single validation failure.
// It is JSON-serialised so the editor can highlight the offending key.
type fieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// GetConfigYAML returns the entire system state — GlobalConfig, all streams,
// all hooks — as one YAML document. The frontend editor uses this to populate
// its initial buffer.
//
// @Summary     Get full system configuration as YAML
// @Description Returns global_config, streams, and hooks bundled in a single YAML document. Pair with PUT /config/yaml to round-trip an editor.
// @Tags        system
// @Produce     application/yaml
// @Success     200 {string} string "YAML document"
// @Failure     500 {object} map[string]any
// @Router      /config/yaml [get].
func (h *ConfigHandler) GetConfigYAML(w http.ResponseWriter, r *http.Request) {
	streams, err := h.streamRepo.List(r.Context(), store.StreamFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_STREAMS_FAILED", err.Error())
		return
	}
	hooks, err := h.hookRepo.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_HOOKS_FAILED", err.Error())
		return
	}
	vodMounts, err := h.vodRepo.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_VOD_FAILED", err.Error())
		return
	}

	full := fullConfig{
		GlobalConfig: h.rtm.CurrentConfig(),
		Streams:      streams,
		Hooks:        hooks,
		VOD:          vodMounts,
	}
	out, err := yaml.Marshal(full)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MARSHAL_FAILED", err.Error())
		return
	}
	w.Header().Set("Content-Type", yamlContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// ReplaceConfigYAML replaces the entire system configuration with the YAML body.
// Performs a full replace, not a merge:
//   - GlobalConfig sections omitted from the body are treated as absent and the
//     corresponding service is stopped.
//   - Streams omitted from the body are stopped and deleted.
//   - Hooks omitted from the body are deleted.
//
// All field errors are collected and returned together so the editor can
// highlight every issue in one round-trip.
//
// @Summary     Replace full system configuration from YAML
// @Description Replaces global_config, streams, and hooks with the request body. Validation errors are returned as a list. On success, the runtime manager + coordinator diff against the previous state and start/stop/restart services accordingly.
// @Tags        system
// @Accept      application/yaml
// @Produce     json
// @Param       body body string true "Full system configuration YAML document"
// @Success     200 {object} map[string]any
// @Failure     400 {object} map[string]any
// @Failure     422 {object} map[string]any
// @Failure     500 {object} map[string]any
// @Router      /config/yaml [put].
func (h *ConfigHandler) ReplaceConfigYAML(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxYAMLBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BODY_TOO_LARGE",
			fmt.Sprintf("request body exceeds %d bytes", maxYAMLBodyBytes))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeError(w, http.StatusBadRequest, "EMPTY_BODY", "YAML body is empty")
		return
	}

	// Strict decode — unknown fields are an error so typos don't silently disappear.
	var newCfg fullConfig
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&newCfg); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_YAML", err.Error())
		return
	}

	// Validate everything up-front; nothing is applied until every section passes.
	if errs := validateFullConfig(&newCfg); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{
				"code":    "VALIDATION_FAILED",
				"message": fmt.Sprintf("%d field(s) failed validation", len(errs)),
				"fields":  errs,
			},
		})
		return
	}

	// Apply order matters:
	//   1. GlobalConfig — bringing services up/down may affect what streams and
	//      hooks can run; do it first so subsequent stream starts see the right
	//      publisher ports, hooks worker pool, etc.
	//   2. Hooks — pure data; the hooks service reads from the repo on each event.
	//   3. Streams — data + lifecycle; depends on the publisher being on the
	//      ports defined in step 1.
	gcfg := newCfg.GlobalConfig
	if gcfg == nil {
		gcfg = &domain.GlobalConfig{}
	}
	if err := h.rtm.Apply(r.Context(), gcfg); err != nil {
		writeError(w, http.StatusInternalServerError, "APPLY_GLOBAL_FAILED", err.Error())
		return
	}

	if err := h.applyHooks(r.Context(), newCfg.Hooks); err != nil {
		writeError(w, http.StatusInternalServerError, "APPLY_HOOKS_FAILED", err.Error())
		return
	}

	if err := h.applyVOD(r.Context(), newCfg.VOD); err != nil {
		writeError(w, http.StatusInternalServerError, "APPLY_VOD_FAILED", err.Error())
		return
	}

	if err := h.applyStreams(r.Context(), newCfg.Streams); err != nil {
		writeError(w, http.StatusInternalServerError, "APPLY_STREAMS_FAILED", err.Error())
		return
	}

	current := h.rtm.CurrentConfig()
	streamsAfter, _ := h.streamRepo.List(r.Context(), store.StreamFilter{})
	hooksAfter, _ := h.hookRepo.List(r.Context())
	vodAfter, _ := h.vodRepo.List(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"global_config": current,
		"streams":       streamsAfter,
		"hooks":         hooksAfter,
		"vod":           vodAfter,
		"ports":         portsFromConfig(current),
	})
}

// applyHooks diffs the desired hook list against what is in the store and
// performs the necessary save/delete operations.
//   - Hooks present in desired but missing from store → Save
//   - Hooks present in store but missing from desired → Delete
//   - Hooks present in both → Save (overwrite is cheap and idempotent)
func (h *ConfigHandler) applyHooks(ctx context.Context, desired []*domain.Hook) error {
	existing, err := h.hookRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("list hooks: %w", err)
	}

	desiredByID := make(map[domain.HookID]*domain.Hook, len(desired))
	for _, hk := range desired {
		if hk == nil {
			continue
		}
		desiredByID[hk.ID] = hk
	}

	for _, old := range existing {
		if old == nil {
			continue
		}
		if _, keep := desiredByID[old.ID]; !keep {
			if err := h.hookRepo.Delete(ctx, old.ID); err != nil {
				return fmt.Errorf("delete hook %q: %w", old.ID, err)
			}
		}
	}

	for _, hk := range desired {
		if hk == nil {
			continue
		}
		if err := h.hookRepo.Save(ctx, hk); err != nil {
			return fmt.Errorf("save hook %q: %w", hk.ID, err)
		}
	}
	return nil
}

// applyVOD diffs the desired VOD mount list against the store and performs
// save/delete to converge. After the store is updated the in-memory registry
// is resynced so the next ingest start sees the new layout.
func (h *ConfigHandler) applyVOD(ctx context.Context, desired []*domain.VODMount) error {
	existing, err := h.vodRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("list vod mounts: %w", err)
	}

	desiredByName := make(map[domain.VODName]*domain.VODMount, len(desired))
	for _, m := range desired {
		if m == nil {
			continue
		}
		desiredByName[m.Name] = m
	}

	for _, old := range existing {
		if old == nil {
			continue
		}
		if _, keep := desiredByName[old.Name]; !keep {
			if err := h.vodRepo.Delete(ctx, old.Name); err != nil {
				return fmt.Errorf("delete vod mount %q: %w", old.Name, err)
			}
		}
	}

	for _, m := range desired {
		if m == nil {
			continue
		}
		if err := h.vodRepo.Save(ctx, m); err != nil {
			return fmt.Errorf("save vod mount %q: %w", m.Name, err)
		}
	}

	mounts, err := h.vodRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("reload vod mounts: %w", err)
	}
	h.vods.Sync(mounts)
	return nil
}

// applyStreams diffs the desired stream set against the store and the
// coordinator's running pipelines, and reconciles each stream individually:
//   - Removed (in store, missing from desired) → Stop pipeline + Delete
//   - Added   (missing from store)             → Save + Start (if not Disabled and has Inputs)
//   - Updated (in both)                        → Save, then Update (if running)
//     or Start (if was disabled/stopped and now enabled)
func (h *ConfigHandler) applyStreams(ctx context.Context, desired []*domain.Stream) error {
	existing, err := h.streamRepo.List(ctx, store.StreamFilter{})
	if err != nil {
		return fmt.Errorf("list streams: %w", err)
	}
	existingByCode := indexStreams(existing)
	desiredByCode := indexStreams(desired)

	if err := h.removeStreams(ctx, existingByCode, desiredByCode); err != nil {
		return err
	}
	return h.upsertStreams(ctx, existingByCode, desiredByCode)
}

func indexStreams(in []*domain.Stream) map[domain.StreamCode]*domain.Stream {
	out := make(map[domain.StreamCode]*domain.Stream, len(in))
	for _, s := range in {
		if s != nil {
			out[s.Code] = s
		}
	}
	return out
}

func (h *ConfigHandler) removeStreams(
	ctx context.Context,
	existing, desired map[domain.StreamCode]*domain.Stream,
) error {
	for code := range existing {
		if _, keep := desired[code]; keep {
			continue
		}
		h.coord.Stop(ctx, code)
		if err := h.streamRepo.Delete(ctx, code); err != nil {
			return fmt.Errorf("delete stream %q: %w", code, err)
		}
	}
	return nil
}

func (h *ConfigHandler) upsertStreams(
	ctx context.Context,
	existing, desired map[domain.StreamCode]*domain.Stream,
) error {
	for code, want := range desired {
		old, exists := existing[code]
		if err := h.streamRepo.Save(ctx, want); err != nil {
			return fmt.Errorf("save stream %q: %w", code, err)
		}
		if err := h.reconcileStreamLifecycle(ctx, old, want, exists); err != nil {
			return err
		}
	}
	return nil
}

func (h *ConfigHandler) reconcileStreamLifecycle(
	ctx context.Context,
	old, want *domain.Stream,
	exists bool,
) error {
	runnable := !want.Disabled && len(want.Inputs) > 0
	switch {
	case exists && h.coord.IsRunning(want.Code):
		if err := h.coord.Update(ctx, old, want); err != nil {
			return fmt.Errorf("update stream %q: %w", want.Code, err)
		}
	case runnable:
		if err := h.coord.Start(ctx, want); err != nil {
			return fmt.Errorf("start stream %q: %w", want.Code, err)
		}
	}
	return nil
}

// validateFullConfig collects validation failures across every section of the
// document. Returning the full list (rather than first-fail) lets the editor
// surface every issue at once instead of forcing the user through many
// round-trips.
func validateFullConfig(c *fullConfig) []fieldError {
	if c == nil {
		return []fieldError{{Path: "", Message: "config is nil"}}
	}
	var errs []fieldError
	if c.GlobalConfig != nil {
		errs = append(errs, validateGlobalConfig(c.GlobalConfig)...)
	}
	errs = append(errs, validateStreams(c.Streams)...)
	errs = append(errs, validateHooks(c.Hooks)...)
	errs = append(errs, validateVOD(c.VOD)...)
	return errs
}

// validateVOD checks each mount individually plus enforces unique names.
// Storage paths must be absolute; the registry skips invalid entries silently
// at sync time, so we surface them up-front during YAML validation instead.
func validateVOD(mounts []*domain.VODMount) []fieldError {
	errs := make([]fieldError, 0, len(mounts))
	seen := make(map[domain.VODName]int, len(mounts))
	for i, m := range mounts {
		base := fmt.Sprintf("vod[%d]", i)
		if m == nil {
			errs = append(errs, fieldError{Path: base, Message: "vod mount entry is null"})
			continue
		}
		if err := domain.ValidateVODName(string(m.Name)); err != nil {
			errs = append(errs, fieldError{Path: base + ".name", Message: err.Error()})
		} else if prev, dup := seen[m.Name]; dup {
			errs = append(errs, fieldError{
				Path:    base + ".name",
				Message: fmt.Sprintf("duplicate vod mount name %q (also at index %d)", m.Name, prev),
			})
		} else {
			seen[m.Name] = i
		}
		if err := m.ValidateStorage(); err != nil {
			errs = append(errs, fieldError{Path: base + ".storage", Message: err.Error()})
		}
	}
	return errs
}

// validateGlobalConfig performs structural checks on the GlobalConfig section.
func validateGlobalConfig(c *domain.GlobalConfig) []fieldError {
	var errs []fieldError
	if c == nil {
		return []fieldError{{Path: "global_config", Message: "config is nil"}}
	}

	if c.Server != nil {
		errs = append(errs, validateServer(c.Server)...)
	}
	if c.Buffer != nil {
		if c.Buffer.Capacity <= 0 {
			errs = append(errs, fieldError{Path: "global_config.buffer.capacity", Message: "must be > 0"})
		}
	}
	if c.Transcoder != nil {
		if c.Transcoder.MaxWorkers < 0 {
			errs = append(errs, fieldError{Path: "global_config.transcoder.max_workers", Message: msgMustBeGEZero})
		}
		if c.Transcoder.MaxRestarts < 0 {
			errs = append(errs, fieldError{Path: "global_config.transcoder.max_restarts", Message: msgMustBeGEZero})
		}
	}
	if c.Publisher != nil {
		errs = append(errs, validatePublisher(c.Publisher)...)
	}
	if c.Hooks != nil {
		if c.Hooks.WorkerCount <= 0 {
			errs = append(errs, fieldError{Path: "global_config.hooks.worker_count", Message: "must be > 0"})
		}
	}
	if c.Log != nil {
		errs = append(errs, validateLog(c.Log)...)
	}
	if c.Ingestor != nil {
		errs = append(errs, validateIngestor(c.Ingestor)...)
	}
	return errs
}

func validateServer(s *config.ServerConfig) []fieldError {
	var errs []fieldError
	if strings.TrimSpace(s.HTTPAddr) == "" {
		errs = append(errs, fieldError{Path: "global_config.server.http_addr", Message: "required (e.g. \":8080\")"})
	} else if _, _, err := net.SplitHostPort(s.HTTPAddr); err != nil {
		errs = append(errs, fieldError{Path: "global_config.server.http_addr", Message: "invalid host:port: " + err.Error()})
	}
	if s.CORS.AllowCredentials {
		for _, o := range s.CORS.AllowedOrigins {
			if o == "*" {
				errs = append(errs, fieldError{
					Path:    "global_config.server.cors.allow_credentials",
					Message: "must be false when allowed_origins contains \"*\"",
				})
				break
			}
		}
	}
	return errs
}

func validatePublisher(p *config.PublisherConfig) []fieldError {
	var errs []fieldError
	errs = append(errs, validatePort("global_config.publisher.rtsp.port_min", p.RTSP.PortMin)...)
	errs = append(errs, validatePort("global_config.publisher.rtmp.port", p.RTMP.Port)...)
	errs = append(errs, validatePort("global_config.publisher.srt.port", p.SRT.Port)...)

	if p.SRT.LatencyMS < 0 {
		errs = append(errs, fieldError{Path: "global_config.publisher.srt.latency_ms", Message: msgMustBeGEZero})
	}
	if p.HLS.LiveSegmentSec < 0 {
		errs = append(errs, fieldError{Path: "global_config.publisher.hls.live_segment_sec", Message: msgMustBeGEZero})
	}
	if p.DASH.LiveSegmentSec < 0 {
		errs = append(errs, fieldError{Path: "global_config.publisher.dash.live_segment_sec", Message: msgMustBeGEZero})
	}
	if p.HLS.Dir != "" && p.DASH.Dir != "" && p.HLS.Dir == p.DASH.Dir {
		errs = append(errs, fieldError{
			Path:    "global_config.publisher.dash.dir",
			Message: "must differ from publisher.hls.dir",
		})
	}
	return errs
}

func validateIngestor(i *config.IngestorConfig) []fieldError {
	var errs []fieldError
	if i.RTMPEnabled {
		if err := validateListenAddr(i.RTMPAddr); err != "" {
			errs = append(errs, fieldError{Path: "global_config.ingestor.rtmp_addr", Message: err})
		}
	}
	if i.SRTEnabled {
		if err := validateListenAddr(i.SRTAddr); err != "" {
			errs = append(errs, fieldError{Path: "global_config.ingestor.srt_addr", Message: err})
		}
	}
	return errs
}

func validateLog(l *config.LogConfig) []fieldError {
	var errs []fieldError
	if l.Level != "" {
		switch strings.ToLower(l.Level) {
		case "debug", "info", "warn", "error":
		default:
			errs = append(errs, fieldError{Path: "global_config.log.level", Message: "must be debug|info|warn|error"})
		}
	}
	if l.Format != "" {
		switch strings.ToLower(l.Format) {
		case "text", "json":
		default:
			errs = append(errs, fieldError{Path: "global_config.log.format", Message: "must be text|json"})
		}
	}
	return errs
}

// validateStreams checks each stream individually plus enforces that codes are
// unique across the whole list (the store can't tell us which one is the dup).
func validateStreams(streams []*domain.Stream) []fieldError {
	var errs []fieldError
	seen := make(map[domain.StreamCode]int, len(streams))
	for i, s := range streams {
		if s == nil {
			errs = append(errs, fieldError{
				Path:    fmt.Sprintf("streams[%d]", i),
				Message: "stream entry is null",
			})
			continue
		}
		base := fmt.Sprintf("streams[%d]", i)
		if err := domain.ValidateStreamCode(string(s.Code)); err != nil {
			errs = append(errs, fieldError{Path: base + ".code", Message: err.Error()})
		} else if prev, dup := seen[s.Code]; dup {
			errs = append(errs, fieldError{
				Path:    base + ".code",
				Message: fmt.Sprintf("duplicate stream code %q (also at index %d)", s.Code, prev),
			})
		} else {
			seen[s.Code] = i
		}
		if err := s.ValidateInputPriorities(); err != nil {
			errs = append(errs, fieldError{Path: base + ".inputs", Message: err.Error()})
		}
		if err := s.ValidateUniqueInputs(); err != nil {
			errs = append(errs, fieldError{Path: base + ".inputs", Message: err.Error()})
		}
	}
	return errs
}

// validateHooks checks each hook individually plus enforces unique IDs across
// the list. Type/target combinations are sanity-checked because a malformed
// hook would silently swallow events at runtime.
func validateHooks(hooks []*domain.Hook) []fieldError {
	errs := make([]fieldError, 0, len(hooks))
	seen := make(map[domain.HookID]int, len(hooks))
	for i, hk := range hooks {
		errs = append(errs, validateHook(hk, i, seen)...)
	}
	return errs
}

func validateHook(hk *domain.Hook, i int, seen map[domain.HookID]int) []fieldError {
	base := fmt.Sprintf("hooks[%d]", i)
	if hk == nil {
		return []fieldError{{Path: base, Message: "hook entry is null"}}
	}
	var errs []fieldError
	errs = append(errs, validateHookID(hk, i, base, seen)...)
	if err := validateHookTypeTarget(hk); err != nil {
		errs = append(errs, fieldError{Path: base + ".target", Message: err.Error()})
	}
	if hk.MaxRetries < 0 {
		errs = append(errs, fieldError{Path: base + ".max_retries", Message: msgMustBeGEZero})
	}
	if hk.TimeoutSec < 0 {
		errs = append(errs, fieldError{Path: base + ".timeout_sec", Message: msgMustBeGEZero})
	}
	if hk.StreamCodes != nil && len(hk.StreamCodes.Only) > 0 && len(hk.StreamCodes.Except) > 0 {
		errs = append(errs, fieldError{
			Path:    base + ".stream_codes",
			Message: "only and except are mutually exclusive",
		})
	}
	return errs
}

func validateHookID(hk *domain.Hook, i int, base string, seen map[domain.HookID]int) []fieldError {
	if strings.TrimSpace(string(hk.ID)) == "" {
		return []fieldError{{Path: base + ".id", Message: "required"}}
	}
	if prev, dup := seen[hk.ID]; dup {
		return []fieldError{{
			Path:    base + ".id",
			Message: fmt.Sprintf("duplicate hook id %q (also at index %d)", hk.ID, prev),
		}}
	}
	seen[hk.ID] = i
	return nil
}

func validateHookTypeTarget(hk *domain.Hook) error {
	if strings.TrimSpace(hk.Target) == "" {
		return errors.New("required")
	}
	switch hk.Type {
	case domain.HookTypeHTTP:
		if !strings.HasPrefix(hk.Target, "http://") && !strings.HasPrefix(hk.Target, "https://") {
			return errors.New("http hook target must be an http(s):// URL")
		}
	case domain.HookTypeKafka:
		// Kafka topic — any non-empty string is acceptable here; broker config
		// lives elsewhere. Just enforce the trim above.
	default:
		return fmt.Errorf("unsupported hook type %q (use http|kafka)", hk.Type)
	}
	return nil
}

// validatePort allows 0 (disabled) or a real TCP/UDP port number.
func validatePort(path string, port int) []fieldError {
	if port == 0 || (port >= 1 && port <= 65535) {
		return nil
	}
	return []fieldError{{Path: path, Message: "must be 0 (disabled) or 1-65535"}}
}

func validateListenAddr(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return "required when service is enabled (e.g. \":1935\")"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "invalid host:port: " + err.Error()
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "port must be numeric"
	}
	_ = host
	return ""
}
