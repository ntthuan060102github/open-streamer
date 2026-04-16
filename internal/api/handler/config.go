package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/hwdetect"
	"github.com/samber/do/v2"
)

// RuntimeConfigManager is the subset of runtime.Manager that the config handler needs.
type RuntimeConfigManager interface {
	CurrentConfig() *domain.GlobalConfig
	Apply(ctx context.Context, newCfg *domain.GlobalConfig) error
}

// publisherPorts exposes listener port configuration so the UI can build output URLs.
type publisherPorts struct {
	HTTPAddr string `json:"http_addr"` // e.g. ":8080"
	RTSPPort int    `json:"rtsp_port"` // e.g. 18554; 0 = disabled
	RTMPPort int    `json:"rtmp_port"` // e.g. 1936; 0 = disabled
	SRTPort  int    `json:"srt_port"`  // e.g. 10000; 0 = disabled
}

// configResponse is the payload returned by GET /config.
type configResponse struct {
	HWAccels           []domain.HWAccel           `json:"hwAccels"`
	VideoCodecs        []domain.VideoCodec        `json:"videoCodecs"`
	AudioCodecs        []domain.AudioCodec        `json:"audioCodecs"`
	OutputProtocols    []string                   `json:"outputProtocols"`
	StreamStatuses     []domain.StreamStatus      `json:"streamStatuses"`
	WatermarkTypes     []domain.WatermarkType     `json:"watermarkTypes"`
	WatermarkPositions []domain.WatermarkPosition `json:"watermarkPositions"`
	Ports              publisherPorts             `json:"ports"`
	GlobalConfig       *domain.GlobalConfig       `json:"globalConfig"`
}

// ConfigHandler serves the GET/POST /config endpoints.
type ConfigHandler struct {
	rtm RuntimeConfigManager
}

// NewConfigHandler creates a ConfigHandler from the DI injector.
func NewConfigHandler(_ do.Injector) (*ConfigHandler, error) {
	return &ConfigHandler{}, nil
}

// SetRuntimeManager injects the RuntimeManager after construction (breaks the circular DI dependency).
func (h *ConfigHandler) SetRuntimeManager(m RuntimeConfigManager) {
	h.rtm = m
}

func portsFromConfig(gcfg *domain.GlobalConfig) publisherPorts {
	var p publisherPorts
	if gcfg.Server != nil {
		p.HTTPAddr = gcfg.Server.HTTPAddr
	}
	if gcfg.Publisher != nil {
		p.RTSPPort = gcfg.Publisher.RTSP.PortMin
		p.RTMPPort = gcfg.Publisher.RTMP.Port
		p.SRTPort = gcfg.Publisher.SRT.Port
	}
	return p
}

// GetConfig returns static enum values, host-detected hardware capabilities,
// current GlobalConfig, and derived publisher ports.
//
// @Summary     Get server configuration.
// @Description Returns available hardware accelerators (OS-detected), static enum lists, publisher listener ports, and the current runtime GlobalConfig.
// @Tags        system
// @Produce     json
// @Success     200 {object} apidocs.ConfigData
// @Router      /config [get].
func (h *ConfigHandler) GetConfig(w http.ResponseWriter, _ *http.Request) {
	gcfg := h.rtm.CurrentConfig()
	resp := configResponse{
		HWAccels: hwdetect.Available(),
		VideoCodecs: []domain.VideoCodec{
			domain.VideoCodecH264,
			domain.VideoCodecH265,
			domain.VideoCodecAV1,
			domain.VideoCodecVP9,
			domain.VideoCodecCopy,
		},
		AudioCodecs: []domain.AudioCodec{
			domain.AudioCodecAAC,
			domain.AudioCodecMP3,
			domain.AudioCodecOpus,
			domain.AudioCodecAC3,
			domain.AudioCodecCopy,
		},
		OutputProtocols: []string{"hls", "dash", "rtmp", "rtsp", "srt"},
		StreamStatuses: []domain.StreamStatus{
			domain.StatusIdle,
			domain.StatusActive,
			domain.StatusDegraded,
			domain.StatusStopped,
		},
		WatermarkTypes: []domain.WatermarkType{
			domain.WatermarkTypeText,
			domain.WatermarkTypeImage,
		},
		WatermarkPositions: []domain.WatermarkPosition{
			domain.WatermarkTopLeft,
			domain.WatermarkTopRight,
			domain.WatermarkBottomLeft,
			domain.WatermarkBottomRight,
			domain.WatermarkCenter,
		},
		Ports:        portsFromConfig(gcfg),
		GlobalConfig: gcfg,
	}
	writeJSON(w, http.StatusOK, resp)
}

// UpdateConfig partially updates the runtime GlobalConfig, persists it to the store,
// and hot-reloads services (start/stop) to match the new configuration.
//
// Only the fields present in the request body are updated; omitted fields keep
// their current values.  Sending a section as JSON null disables that service.
//
// @Summary     Partially update server configuration.
// @Description Merges the request body into the current GlobalConfig and applies it — services are started or stopped to match.
// @Tags        system
// @Accept      json
// @Produce     json
// @Param       body body domain.GlobalConfig true "Partial configuration to merge"
// @Success     200 {object} apidocs.ConfigUpdateResponse
// @Failure     400 {object} apidocs.ErrorResponse
// @Failure     500 {object} apidocs.ErrorResponse
// @Router      /config [post].
func (h *ConfigHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	// Deep-copy the current config so json.Decode merges on top of it.
	current := h.rtm.CurrentConfig()
	raw, err := json.Marshal(current)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MARSHAL_FAILED", err.Error())
		return
	}
	var merged domain.GlobalConfig
	if err := json.Unmarshal(raw, &merged); err != nil {
		writeError(w, http.StatusInternalServerError, "COPY_FAILED", err.Error())
		return
	}

	// Unmarshal the request body on top: present fields overwrite, omitted fields stay.
	if err := json.NewDecoder(r.Body).Decode(&merged); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
		return
	}

	if err := h.rtm.Apply(r.Context(), &merged); err != nil {
		writeError(w, http.StatusInternalServerError, "APPLY_FAILED", err.Error())
		return
	}

	// Return the freshly applied config.
	gcfg := h.rtm.CurrentConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"globalConfig": gcfg,
		"ports":        portsFromConfig(gcfg),
	})
}
