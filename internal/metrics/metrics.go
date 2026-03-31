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
	// Ingestor
	IngestorBytesTotal   *prometheus.CounterVec
	IngestorPacketsTotal *prometheus.CounterVec
	IngestorErrorsTotal  *prometheus.CounterVec

	// Stream Manager
	ManagerFailoversTotal *prometheus.CounterVec
	ManagerInputHealth    *prometheus.GaugeVec

	// Transcoder
	TranscoderWorkersActive *prometheus.GaugeVec
	TranscoderRestartsTotal *prometheus.CounterVec

	// Buffer
	BufferPackets *prometheus.GaugeVec

	// Publisher
	PublisherClientsActive *prometheus.GaugeVec
	PublisherSegmentsTotal *prometheus.CounterVec

	// Hooks
	HooksDeliveryFailedTotal *prometheus.CounterVec
}

// New registers all metrics and returns a Metrics instance.
func New(i do.Injector) (*Metrics, error) {
	m := &Metrics{
		IngestorBytesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_bytes_total",
			Help: "Total bytes ingested per stream.",
		}, []string{"stream_code", "protocol"}),

		IngestorPacketsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_packets_total",
			Help: "Total MPEG-TS packets received per stream.",
		}, []string{"stream_code", "protocol"}),

		IngestorErrorsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_errors_total",
			Help: "Total ingestion errors per stream.",
		}, []string{"stream_code", "reason"}),

		ManagerFailoversTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_manager_failovers_total",
			Help: "Total input failover events per stream.",
		}, []string{"stream_code"}),

		ManagerInputHealth: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_manager_input_health",
			Help: "Input health: 1=alive, 0=dead.",
		}, []string{"stream_code", "input_priority"}),

		TranscoderWorkersActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_transcoder_workers_active",
			Help: "Number of active FFmpeg transcoding workers.",
		}, []string{"stream_code"}),

		TranscoderRestartsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_transcoder_restarts_total",
			Help: "Total FFmpeg process restarts per stream.",
		}, []string{"stream_code"}),

		BufferPackets: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_buffer_packets",
			Help: "Current number of packets in the ring buffer per stream.",
		}, []string{"stream_code"}),

		PublisherClientsActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_publisher_clients_active",
			Help: "Number of active viewer connections per stream.",
		}, []string{"stream_code", "protocol"}),

		PublisherSegmentsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_publisher_segments_total",
			Help: "Total HLS segments written per stream.",
		}, []string{"stream_code", "profile"}),

		HooksDeliveryFailedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_hooks_delivery_failed_total",
			Help: "Total hook delivery failures after all retries.",
		}, []string{"hook_id", "hook_type"}),
	}

	return m, nil
}
