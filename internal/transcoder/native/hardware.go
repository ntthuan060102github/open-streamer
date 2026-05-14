package native

import (
	"fmt"

	"github.com/asticode/go-astiav"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// hwDeviceTypeFor maps Open-Streamer's HWAccel enum to astiav's
// HardwareDeviceType. Returns HardwareDeviceTypeNone for HWAccelNone
// or an empty value so callers can short-circuit hardware init.
//
// Kept private to the package — the mapping is an implementation
// detail of the native backend. Callers outside the package configure
// pipelines via domain.HWAccel, not astiav types.
func hwDeviceTypeFor(hw domain.HWAccel) astiav.HardwareDeviceType {
	switch hw {
	case domain.HWAccelNVENC:
		return astiav.HardwareDeviceTypeCUDA
	case domain.HWAccelQSV:
		return astiav.HardwareDeviceTypeQSV
	case domain.HWAccelVAAPI:
		return astiav.HardwareDeviceTypeVAAPI
	case domain.HWAccelVideoToolbox:
		return astiav.HardwareDeviceTypeVideoToolbox
	}
	return astiav.HardwareDeviceTypeNone
}

// initHardware allocates the shared hardware-acceleration context the
// decoder, every filter graph, and every encoder will reuse. Sharing
// one context means a frame's hardware-backed buffers can flow stage
// to stage without forcing a GPU→CPU→GPU round-trip in between.
//
// HWAccelNone (and any unknown HW value the enum maps to
// HardwareDeviceTypeNone) skips allocation entirely; p.hwDevice stays
// nil and every downstream stage will run on CPU surfaces.
//
// The empty device string lets libavutil pick the first matching
// device — matches the existing FFmpeg CLI backend's behaviour with
// `-hwaccel cuda` (no `-init_hw_device cuda:0` override). A future
// `cfg.DeviceID` knob would feed in here; out of scope for v1.
func (p *Pipeline) initHardware() error {
	hwType := hwDeviceTypeFor(p.cfg.HW)
	if hwType == astiav.HardwareDeviceTypeNone {
		return nil
	}

	ctx, err := astiav.CreateHardwareDeviceContext(hwType, "", nil, 0)
	if err != nil {
		return fmt.Errorf("native pipeline: create hardware device context %s: %w", hwType, err)
	}
	p.hwDevice = ctx
	return nil
}
