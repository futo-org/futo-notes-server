package hostedjobs

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// Item kinds the storage-backed jobs count.
const (
	itemsProcessed = "processed"
	itemsDeleted   = "deleted"
	itemsFailed    = "failed"
)

// Metrics counts what the storage-backed jobs process. Runs and their
// outcomes are recorded through RecordRun.
type Metrics struct {
	items otelmetric.Int64Counter
}

// NewMetrics creates the job instruments on meter.
func NewMetrics(meter otelmetric.Meter) (*Metrics, error) {
	items, err := meter.Int64Counter("jobs.items",
		otelmetric.WithDescription("Items background jobs processed, deleted or failed to delete, by job"))
	if err != nil {
		return nil, fmt.Errorf("creating job item counter: %w", err)
	}
	return &Metrics{items: items}, nil
}

func (m *Metrics) add(ctx context.Context, job string, counts map[string]int) {
	for kind, n := range counts {
		if n > 0 {
			m.items.Add(context.WithoutCancel(ctx), int64(n),
				otelmetric.WithAttributes(attribute.String("job", job), attribute.String("kind", kind)))
		}
	}
}
