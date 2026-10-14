package grafana

import (
	"math"
	"strconv"
	"time"
)

// maxDataPoints caps the number of points Grafana is asked to return.
const maxDataPoints = 11000

// QueryParams is a fully resolved Loki query, ready to be turned into the
// /api/ds/query request body.
type QueryParams struct {
	DatasourceUID string
	LogQL         string
	From, To      time.Time
	Limit         int           // already clamped
	Direction     string        // "backward" | "forward"
	Step          time.Duration // already resolved (>0)
}

// BuildPayload renders the /api/ds/query request body for p.
func BuildPayload(p QueryParams) map[string]any {
	span := p.To.Sub(p.From)
	points := maxDataPoints
	if p.Step > 0 {
		if n := int(math.Ceil(float64(span) / float64(p.Step))); n < maxDataPoints {
			points = n
		}
	}
	return map[string]any{
		"from": strconv.FormatInt(p.From.UnixMilli(), 10),
		"to":   strconv.FormatInt(p.To.UnixMilli(), 10),
		"queries": []any{
			map[string]any{
				"refId":         "A",
				"datasource":    map[string]any{"type": "loki", "uid": p.DatasourceUID},
				"expr":          p.LogQL,
				"queryType":     "range",
				"maxLines":      p.Limit,
				"direction":     p.Direction,
				"step":          strconv.Itoa(int(p.Step.Seconds())) + "s",
				"intervalMs":    p.Step.Milliseconds(),
				"maxDataPoints": points,
			},
		},
	}
}
