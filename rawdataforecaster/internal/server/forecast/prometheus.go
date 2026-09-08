package forecast

import "github.com/prometheus/client_golang/prometheus"

var areaCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "forti_requested_areas_total",
		Help: "What areas are requested",
	},
	[]string{"area"},
)

var distanceHistogram = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "forti_distance_to_selected_grid_point",
		Help:    "Distance between requested and selected grid point",
		Buckets: []float64{1_000, 5_000, 10_000, 25_000, 100_000},
	},
	[]string{"area"},
)

var fortiAvailableLatest = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "forti_available_latest",
		Help: "Available version for each area (being loaded)",
	},
	[]string{"area"},
)

var fortiAvailableUpdated = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "forti",
		Subsystem: "available",
		Name:      "updated",
		Help:      "Update time of latest version of a dataset",
	},
	[]string{"area"},
)

var fortiActiveLatest = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "forti_active_latest",
		Help: "Active version for each area",
	},
	[]string{"area"},
)

var fortiActiveUpdated = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "forti",
		Subsystem: "active",
		Name:      "updated",
		Help:      "Update time of latest version of a dataset",
	},
	[]string{"area"},
)

func init() {
	prometheus.MustRegister(
		areaCounter,
		distanceHistogram,
		fortiActiveLatest,
		fortiActiveUpdated,
		fortiAvailableLatest,
		fortiAvailableUpdated,
	)
}

// forgetArea removes the series belonging to an area that is no longer
// configured.
//
// Without this, forti_active_latest keeps reporting a version for an area the
// service stopped serving the moment it was removed — a gauge that says a
// dataset is active when it is not is worse than no gauge at all.
//
// areaCounter and distanceHistogram are deliberately left alone: they count
// what was requested, and that did happen.
func forgetArea(area string) {
	labels := prometheus.Labels{"area": area}
	for _, gauge := range []*prometheus.GaugeVec{
		fortiActiveLatest,
		fortiActiveUpdated,
		fortiAvailableLatest,
		fortiAvailableUpdated,
	} {
		gauge.Delete(labels)
	}
}
