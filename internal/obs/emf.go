// Package obs emits CloudWatch metrics through the Embedded Metric Format.
package obs

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Namespace of all custom metrics.
const Namespace = "MicrovmGithubRunners"

// Metric emits one metric.
func Metric(log *slog.Logger, name string, value float64) {
	Metrics(log, map[string]float64{name: value})
}

// Metrics emits several Count metrics in one EMF record, dimensioned by repository.
func Metrics(log *slog.Logger, values map[string]float64) {
	defs := make([]map[string]string, 0, len(values))
	attrs := []any{
		"_aws", map[string]any{
			"Timestamp": time.Now().UnixMilli(),
			"CloudWatchMetrics": []map[string]any{{
				"Namespace":  Namespace,
				"Dimensions": [][]string{{"Repository"}},
				"Metrics":    &defs,
			}},
		},
		"Repository", os.Getenv("REPOSITORY"),
	}
	for k, v := range values {
		defs = append(defs, map[string]string{"Name": k, "Unit": "Count"})
		attrs = append(attrs, k, v)
	}
	log.Log(context.Background(), slog.LevelInfo, "metrics", attrs...)
}
