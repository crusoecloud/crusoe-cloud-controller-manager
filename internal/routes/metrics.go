package routes

import (
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const metricSubsystem = "crusoe"

// reconcile result label values.
const (
	resultSuccess = "success"
	resultError   = "error"
	resultRequeue = "requeue"
)

//nolint:gochecknoglobals // component-base metrics are package-level by convention
var (
	metricReconcileTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      metricSubsystem,
			Name:           "pod_cidr_allocation_reconcile_total",
			Help:           "Count of pod CIDR allocation reconcile outcomes.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"result"},
	)

	metricProvisionSeconds = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      metricSubsystem,
			Name:           "pod_cidr_allocation_provision_seconds",
			Help:           "Time from create issued to operation SUCCEEDED, in seconds.",
			Buckets:        []float64{1, 2.5, 5, 10, 30, 60, 120, 300, 600},
			StabilityLevel: metrics.ALPHA,
		},
	)

	metricAllocationsDesired = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      metricSubsystem,
			Name:           "pod_cidr_allocations_desired",
			Help:           "Desired pod CIDR allocations (CiliumNodes with a routed podCIDR), set each reaper pass.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	metricAllocationsActual = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      metricSubsystem,
			Name:           "pod_cidr_allocations_actual",
			Help:           "Actual pod CIDR allocations in the reservation, set each reaper pass.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	metricConflictTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem: metricSubsystem,
			Name:      "pod_cidr_allocation_conflict_total",
			Help: "Count of DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE conflicts " +
				"(the /24-reuse race); a sustained rate indicates a leaked allocation.",
			StabilityLevel: metrics.ALPHA,
		},
	)
)

//nolint:gochecknoinits // legacyregistry.MustRegister must run at package init
func init() {
	legacyregistry.MustRegister(
		metricReconcileTotal,
		metricProvisionSeconds,
		metricAllocationsDesired,
		metricAllocationsActual,
		metricConflictTotal,
	)
}
