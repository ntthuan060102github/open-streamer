package domain

// Default* constants are the runtime-applied values when the matching
// configuration field is left zero / empty by the user. They are the SINGLE
// SOURCE OF TRUTH for implicit defaults — every consumer (handler, packager,
// transcoder, validator) must reference these instead of inlining its own
// literal so the API's "effective" view stays accurate.
//
// When changing a default here, audit references with:
//
//	grep -RIn 'Default[A-Z][A-Za-z]*' internal/ config/
//
// to make sure no stale literal still exists in a service module.
const (
	// DefaultBufferCapacity is the per-subscriber channel buffer size in
	// MPEG-TS packets. 1024 packets ≈ 1 MiB ≈ ~1.5s of 1080p60 video at
	// 5Mbps — enough headroom for HLS pull bursts (one segment per Read)
	// without growing memory unbounded across many subscribers.
	DefaultBufferCapacity = 1024

	// DefaultInputPacketTimeoutSec is the manager's failover threshold: the
	// maximum gap (in seconds) without a successful read on the active
	// input before it is marked failed and the next-priority input takes
	// over.
	DefaultInputPacketTimeoutSec = 30

	// DefaultLiveSegmentSec is the live HLS / DASH segment target length
	// in seconds when the operator leaves the field unset.
	DefaultLiveSegmentSec = 2
	// DefaultLiveWindow is the segment count in the sliding playlist
	// window (HLS / DASH live).
	DefaultLiveWindow = 12
	// DefaultLiveHistory is extra segments retained on disk after they
	// leave the live window — 0 = drop immediately after sliding out.
	DefaultLiveHistory = 0

	// DefaultRTMPPort is the IANA-assigned RTMP listener port used when an
	// RTMP listener is enabled with no explicit port.
	DefaultRTMPPort = 1935
	// DefaultRTSPPort is the IANA-assigned RTSP listener port.
	DefaultRTSPPort = 554
	// DefaultRTSPTransport is the underlying transport when RTSPListenerConfig
	// leaves Transport empty — "tcp" interleaves RTP into the control channel
	// and is firewall-friendly.
	DefaultRTSPTransport = "tcp"
	// DefaultSRTPort is the conventional SRT listener port.
	DefaultSRTPort = 9999

	// DefaultDVRSegmentDuration is the DVR segment length in seconds when
	// StreamDVRConfig.SegmentDuration is zero.
	DefaultDVRSegmentDuration = 4
	// DefaultDVRRoot is the on-disk root directory used as the parent of
	// "{DefaultDVRRoot}/{streamCode}" when StreamDVRConfig.StoragePath is
	// empty.
	DefaultDVRRoot = "./out/dvr"

	// DefaultPushTimeoutSec is the publish handshake budget for an outbound
	// push destination when PushDestination.TimeoutSec is zero.
	DefaultPushTimeoutSec = 10
	// DefaultPushRetryTimeoutSec is the delay between retry attempts.
	DefaultPushRetryTimeoutSec = 5

	// DefaultHookMaxRetries is the per-hook retry budget when the user
	// leaves Hook.MaxRetries=0.
	DefaultHookMaxRetries = 3
	// DefaultHookTimeoutSec is the per-attempt delivery timeout when
	// Hook.TimeoutSec=0.
	DefaultHookTimeoutSec = 10

	// DefaultVideoBitrateK is the fallback target video bitrate (kbps)
	// when a profile leaves Bitrate=0.
	DefaultVideoBitrateK = 2500

	// DefaultVideoResizeMode is the fallback for VideoProfile.ResizeMode.
	DefaultVideoResizeMode = ResizeModePad

	// DefaultAudioCodec is the audio codec emitted when the user leaves
	// AudioTranscodeConfig.Codec empty (or set to "copy" while
	// Audio.Copy=false, which is meaningless and treated the same).
	DefaultAudioCodec AudioCodec = AudioCodecAAC
	// DefaultAudioBitrateK is the audio bitrate (kbps) when
	// AudioTranscodeConfig.Bitrate is zero.
	DefaultAudioBitrateK = 128

	// DefaultHWAccel is the fallback when TranscoderGlobalConfig.HW is
	// empty — CPU-only encoding via libx264.
	DefaultHWAccel HWAccel = HWAccelNone

	// DefaultHLSPlaylistTimeoutSec is the upstream HLS pull playlist GET
	// timeout (seconds) when InputNetConfig.ConnectTimeoutSec is zero.
	DefaultHLSPlaylistTimeoutSec = 15
	// DefaultHLSSegmentTimeoutSec is the upstream HLS segment GET timeout
	// floor — segments can be many MB so this is held above the playlist
	// timeout.
	DefaultHLSSegmentTimeoutSec = 60
)
