package main

import "sort"

// Row is one measured (target, scenario) pair, written as one line of
// rows.jsonl. The JSON keys are fixed by contract C8.
type Row struct {
	Target             string  `json:"target"`
	Scenario           string  `json:"scenario"`
	Concurrency        int     `json:"concurrency"`
	N                  int     `json:"n"`
	Errors             int     `json:"errors"`
	Throttled          int     `json:"throttled"`
	P50MS              float64 `json:"p50_ms"`
	P95MS              float64 `json:"p95_ms"`
	MaxMS              float64 `json:"max_ms"`
	RPS                float64 `json:"rps"`
	RespBytesP50       float64 `json:"resp_bytes_p50"`
	PointsPerSeriesMax int     `json:"points_per_series_max"`
	LinesP50           float64 `json:"lines_p50"`
}

// nearestRank returns the q-quantile (0 < q <= 1) of samples with the
// nearest-rank method: the smallest value whose rank covers ceil(q*n).
func nearestRank(samples []float64, q float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)

	rank := int(q * float64(len(sorted)))
	if q*float64(len(sorted)) > float64(rank) {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// maxOf returns the largest sample, or 0 for none.
func maxOf(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	largest := samples[0]
	for _, sample := range samples[1:] {
		if sample > largest {
			largest = sample
		}
	}
	return largest
}
