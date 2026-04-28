// Package metrics registers all Prometheus collectors for Open Streamer.
// Naming convention: open_streamer_<module>_<metric>_<unit>
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/samber/do/v2"
)

// Metrics holds all registered Prometheus collectors.
type Metrics struct {
	// ── Ingestor ─────────────────────────────────────────────────────────────
	// IngestorBytesTotal counts raw bytes received from each input source.
	IngestorBytesTotal *prometheus.CounterVec
	// IngestorPacketsTotal counts MPEG-TS packets written to the buffer.
	IngestorPacketsTotal *prometheus.CounterVec
	// IngestorErrorsTotal counts transient read/reconnect errors per stream.
	IngestorErrorsTotal *prometheus.CounterVec

	// ── Stream Manager ────────────────────────────────────────────────────────
	// ManagerFailoversTotal counts input source switches (primary → backup).
	ManagerFailoversTotal *prometheus.CounterVec
	// ManagerInputHealth is 1 when an input is delivering packets, 0 when degraded.
	ManagerInputHealth *prometheus.GaugeVec

	// ── Transcoder ────────────────────────────────────────────────────────────
	// TranscoderWorkersActive is the number of active FFmpeg processes per stream.
	TranscoderWorkersActive *prometheus.GaugeVec
	// TranscoderRestartsTotal counts FFmpeg crash-restarts per stream.
	TranscoderRestartsTotal *prometheus.CounterVec
	// TranscoderQualitiesActive is the number of active ABR renditions per stream.
	TranscoderQualitiesActive *prometheus.GaugeVec

	// ── DVR ───────────────────────────────────────────────────────────────────
	// DVRSegmentsWrittenTotal counts TS segments flushed to disk per stream.
	DVRSegmentsWrittenTotal *prometheus.CounterVec
	// DVRBytesWrittenTotal counts bytes written to disk per stream.
	DVRBytesWrittenTotal *prometheus.CounterVec

	// ── Stream lifecycle ──────────────────────────────────────────────────────
	// StreamStartTimeSeconds is the Unix timestamp when a stream pipeline was
	// last started. 0 / absent means the stream is not currently running.
	// Uptime in Grafana: time() - open_streamer_stream_start_time_seconds
	StreamStartTimeSeconds *prometheus.GaugeVec

	// ── Buffer Hub ────────────────────────────────────────────────────────────
	// BufferDropsTotal counts MPEG-TS packets dropped at the Buffer Hub
	// fan-out because a subscriber's channel was full. This is the canonical
	// signal that the "write never blocks" invariant is shielding the writer
	// from a slow consumer — alert on `rate(...) > 0` to catch quality
	// regressions before users complain.
	BufferDropsTotal *prometheus.CounterVec

	// ── Publisher ─────────────────────────────────────────────────────────────
	// PublisherSegmentsTotal counts HLS / DASH media segments successfully
	// packaged. format=hls|dash; profile is the rendition slug (track_1…) or
	// "main" for single-rendition streams. Rate sanity-checks output bitrate.
	PublisherSegmentsTotal *prometheus.CounterVec
	// PublisherPushState reports the state of each RTMP/RTMPS push destination
	// as a small enum cast to int: 0=failed, 1=reconnecting, 2=starting, 3=active.
	// Per-destination labels let alerts target individual sinks.
	PublisherPushState *prometheus.GaugeVec

	// ── Sessions ──────────────────────────────────────────────────────────────
	// SessionsActive is the number of currently-tracked play sessions per
	// stream and protocol. Reset to 0 on restart (in-memory tracker), but
	// converges back as viewers reconnect — Prometheus gauge semantics
	// match this exactly.
	SessionsActive *prometheus.GaugeVec
	// SessionsOpenedTotal counts every session creation per stream and proto.
	// Pair with rate() / increase() over a window for "viewer churn".
	SessionsOpenedTotal *prometheus.CounterVec
	// SessionsClosedTotal counts session terminations per stream, proto and
	// reason (idle, client_gone, kicked, shutdown). Pair with opened_total
	// to compute average session lifetime; spike of `kicked` could indicate
	// abuse mitigation kicking in.
	SessionsClosedTotal *prometheus.CounterVec

	// ── Hooks ─────────────────────────────────────────────────────────────────
	// HooksDeliveryTotal counts hook deliveries per hook, type and outcome
	// (success / failure). For HTTP hooks each batch counts as ONE delivery
	// regardless of batch size — divide events_dropped_total by this to
	// derive lost-event rate.
	HooksDeliveryTotal *prometheus.CounterVec
	// HooksEventsDroppedTotal counts events evicted from the per-hook batch
	// queue when it overflowed `BatchMaxQueueItems`. Non-zero rate means a
	// downstream HTTP target is stalled long enough to threaten data loss —
	// alert immediately.
	HooksEventsDroppedTotal *prometheus.CounterVec
	// HooksBatchQueueDepth is the current per-hook in-memory queue depth.
	// Together with HooksEventsDroppedTotal this gives the early-warning
	// signal: queue rising → target back-pressuring; queue at cap → drops imminent.
	HooksBatchQueueDepth *prometheus.GaugeVec

	// ── DVR ───────────────────────────────────────────────────────────────────
	// DVRRecordingActive is 1 while a DVR recording is being written for a
	// stream, 0 otherwise. Combine with rate(DVRSegmentsWrittenTotal) to
	// alert on "DVR enabled but no segments flowing".
	DVRRecordingActive *prometheus.GaugeVec
}

// New registers all metrics and returns a Metrics instance.
func New(i do.Injector) (*Metrics, error) {
	_ = i
	m := &Metrics{
		IngestorBytesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_bytes_total",
			Help: "Total bytes ingested from pull/push sources per stream and protocol.",
		}, []string{"stream_code", "protocol"}),

		IngestorPacketsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_packets_total",
			Help: "Total MPEG-TS packets written to the buffer per stream and protocol.",
		}, []string{"stream_code", "protocol"}),

		IngestorErrorsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_errors_total",
			Help: "Total ingestion errors per stream, categorised by reason (reconnect, failover).",
		}, []string{"stream_code", "reason"}),

		ManagerFailoversTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_manager_failovers_total",
			Help: "Total input source switches (primary degraded → backup activated) per stream.",
		}, []string{"stream_code"}),

		ManagerInputHealth: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_manager_input_health",
			Help: "Input health per stream and priority: 1 = delivering packets, 0 = degraded.",
		}, []string{"stream_code", "input_priority"}),

		TranscoderWorkersActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_transcoder_workers_active",
			Help: "Number of FFmpeg encoder processes currently running per stream.",
		}, []string{"stream_code"}),

		TranscoderRestartsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_transcoder_restarts_total",
			Help: "Total FFmpeg process crash-restarts per stream.",
		}, []string{"stream_code"}),

		TranscoderQualitiesActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_transcoder_qualities_active",
			Help: "Number of active ABR rendition profiles per stream.",
		}, []string{"stream_code"}),

		DVRSegmentsWrittenTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_dvr_segments_written_total",
			Help: "Total TS segments successfully written to disk per stream.",
		}, []string{"stream_code"}),

		DVRBytesWrittenTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_dvr_bytes_written_total",
			Help: "Total bytes written to disk by the DVR per stream.",
		}, []string{"stream_code"}),

		StreamStartTimeSeconds: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_stream_start_time_seconds",
			Help: "Unix timestamp when the stream pipeline was last started. Absent when stopped. Uptime = time() - this value.",
		}, []string{"stream_code"}),

		BufferDropsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_buffer_drops_total",
			Help: "Total MPEG-TS packets dropped by the Buffer Hub fan-out because a subscriber channel was full.",
		}, []string{"stream_code"}),

		PublisherSegmentsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_publisher_segments_total",
			Help: "Total HLS/DASH segments packaged per stream, format and rendition profile.",
		}, []string{"stream_code", "format", "profile"}),

		PublisherPushState: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_publisher_push_state",
			Help: "RTMP/RTMPS push destination state: 0=failed, 1=reconnecting, 2=starting, 3=active.",
		}, []string{"stream_code", "dest_url"}),

		SessionsActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_sessions_active",
			Help: "Currently-tracked play sessions per stream and protocol.",
		}, []string{"stream_code", "proto"}),

		SessionsOpenedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_sessions_opened_total",
			Help: "Total play sessions opened per stream and protocol.",
		}, []string{"stream_code", "proto"}),

		SessionsClosedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_sessions_closed_total",
			Help: "Total play sessions closed per stream, protocol and reason (idle|client_gone|kicked|shutdown).",
		}, []string{"stream_code", "proto", "reason"}),

		HooksDeliveryTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_hooks_delivery_total",
			Help: "Total hook delivery attempts per hook, type and outcome (success|failure). For HTTP hooks each batch is one delivery.",
		}, []string{"hook_id", "hook_type", "outcome"}),

		HooksEventsDroppedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_hooks_events_dropped_total",
			Help: "Total events evicted from the per-hook batch queue on overflow (target unreachable too long).",
		}, []string{"hook_id"}),

		HooksBatchQueueDepth: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_hooks_batch_queue_depth",
			Help: "Current per-hook in-memory batch queue depth.",
		}, []string{"hook_id"}),

		DVRRecordingActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_dvr_recording_active",
			Help: "1 while a DVR recording is being written for the stream, 0 otherwise.",
		}, []string{"stream_code"}),
	}

	return m, nil
}
