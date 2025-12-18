package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	//nolint: revive
	EventsProcessedOk = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "deptracker_events_processed_ok",
			Help: "The total number of successful events",
		},
		[]string{"event_type"},
	)

	//nolint: revive
	EventsProcessedFailed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "deptracker_events_processed_failed",
			Help: "The total number of failed events",
		},
		[]string{"event_type"},
	)

	//nolint: revive
	EventsProcessedTimer = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "deptracker_events_processed_timer",
			Help: "The duration (seconds) for processing k8s events",
		},
		[]string{"status"},
	)

	//nolint: revive
	PostDeploymentRecordTimer = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name: "deptracker_post_deployment_record_timer",
			Help: "The duration (seconds) for posting data to the GitHub API",
		},
	)

	//nolint: revive
	PostDeploymentRecordOk = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "deptracker_post_record_ok",
			Help: "The total number of successful posts",
		},
	)

	//nolint: revive
	PostDeploymentRecordSoftFail = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "deptracker_post_record_soft_fail",
			Help: "The total number of soft (recoverable) post failures",
		},
	)

	//nolint: revive
	PostDeploymentRecordHardFail = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "deptracker_post_record_hard_fail",
			Help: "The total number of hard post failures",
		},
	)
)
