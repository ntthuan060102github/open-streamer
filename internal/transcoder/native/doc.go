// Package native is an in-process transcoder backend that links libavcodec
// / libavformat / libavfilter through asticode/go-astiav instead of
// spawning an FFmpeg CLI subprocess per stream.
//
// # Motivation
//
// The existing transcoder spawns one ffmpeg(1) process per stream. With
// 21 streams × 2 profiles on Tesla T4, encoder load pegs 100 % vs
// Flussonic's 64 % on identical config. Investigation pinpointed the
// gap to FFmpeg CLI subprocess overhead: pipe I/O between Go and the
// child, 42 separate NVENC driver contexts, no cross-stream frame
// pacing coordination. The fix is architectural — replace the
// subprocess with an in-process pipeline that talks to libav directly.
//
// # Boundary
//
// The package exposes a Pipeline type with a small surface:
//
//	NewPipeline(cfg Config) (*Pipeline, error)
//	(*Pipeline).Run(ctx context.Context) error
//	(*Pipeline).Close() error
//
// One Pipeline corresponds to one logical Open-Streamer stream — one
// input source, one shared decode, N profile outputs (the "multi" mode
// equivalent of the FFmpeg CLI path).
//
// Service integration happens through a Backend interface at the
// transcoder.Service layer (added later). During migration the existing
// subprocess backend stays available so individual streams can switch
// to the native backend with a config flag, with rollback by flipping
// it back.
//
// # Pipeline stages
//
// Per-stream pipeline (mirroring the FFmpeg CLI multi-output args):
//
//	demux (mpegts) → decode (NVDEC) → ┬ filter (yadif_cuda → scale_cuda 720p) → encode (h264_nvenc) → mux (mpegts) → output 1
//	                                  └ filter (yadif_cuda → scale_cuda 480p) → encode (h264_nvenc) → mux (mpegts) → output 2
//
// All stages share one HardwareDeviceContext (CUDA) so frame refs pass
// between stages without GPU↔CPU bridges. Profiles share the decode —
// matching the current `multi` mode's NVDEC count saving.
//
// # Status
//
// Skeleton. All stage entry points are stubbed with TODO markers
// pointing at the astiav APIs they need to call. The package compiles
// and has its lifecycle scaffold tested, so the actual integration
// work happens in focused commits per stage.
package native
