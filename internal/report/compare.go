package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

const maxCompareFileBytes = 32 << 20

// CompareOptions defines optional regression thresholds for lemmings compare.
type CompareOptions struct {
	MaxFailureDelta float64 // max allowed increase in failure rate (0..1), <0 disables
	MaxP95DeltaMS   float64 // max allowed increase in p95 latency (ms), <=0 disables
	MaxRescuedDrop  float64 // max allowed drop in rescued % (0..100), <0 disables
}

// PathDiff compares one path's latency and failure rate across two runs.
type PathDiff struct {
	URL         string  `json:"url"`
	OldHits     int64   `json:"old_hits"`
	NewHits     int64   `json:"new_hits"`
	OldP95MS    float64 `json:"old_p95_ms"`
	NewP95MS    float64 `json:"new_p95_ms"`
	P95DeltaMS  float64 `json:"p95_delta_ms"`
	OldFailed   int64   `json:"old_failed"`
	NewFailed   int64   `json:"new_failed"`
	FailedDelta int64   `json:"failed_delta"`
}

// CompareResult summarizes differences and regression gates between two runs.
type CompareResult struct {
	OldGeneratedAt       time.Time    `json:"old_generated_at"`
	NewGeneratedAt       time.Time    `json:"new_generated_at"`
	Target               string       `json:"target"`
	OldRescuedPct        float64      `json:"old_rescued_percent"`
	NewRescuedPct        float64      `json:"new_rescued_percent"`
	RescuedDeltaPct      float64      `json:"rescued_delta_percent"`
	OldFailureRate       float64      `json:"old_failure_rate"`
	NewFailureRate       float64      `json:"new_failure_rate"`
	FailureRateDelta     float64      `json:"failure_rate_delta"`
	OldP50MS             float64      `json:"old_p50_ms"`
	NewP50MS             float64      `json:"new_p50_ms"`
	OldP95MS             float64      `json:"old_p95_ms"`
	NewP95MS             float64      `json:"new_p95_ms"`
	P95DeltaMS           float64      `json:"p95_delta_ms"`
	OldP99MS             float64      `json:"old_p99_ms"`
	NewP99MS             float64      `json:"new_p99_ms"`
	OldAssetP95MS        float64      `json:"old_asset_p95_ms,omitempty"`
	NewAssetP95MS        float64      `json:"new_asset_p95_ms,omitempty"`
	AssetP95DeltaMS      float64      `json:"asset_p95_delta_ms,omitempty"`
	OldQueueHeld         int64        `json:"old_queue_held,omitempty"`
	NewQueueHeld         int64        `json:"new_queue_held,omitempty"`
	QueueHeldDelta       int64        `json:"queue_held_delta,omitempty"`
	NewFingerprints      []string     `json:"new_error_fingerprints,omitempty"`
	ResolvedFingerprints []string     `json:"resolved_error_fingerprints,omitempty"`
	Paths                []PathDiff   `json:"paths,omitempty"`
	Gates                []GateResult `json:"gates,omitempty"`
	Regressed            bool         `json:"regressed"`
}

// LoadJSONFile reads and decodes a Lemmings JSON report from path.
func LoadJSONFile(path string) (Data, error) {
	f, err := os.Open(path)
	if err != nil {
		return Data{}, fmt.Errorf("open report %q: %w", path, err)
	}
	defer f.Close() // read-only handle

	b, err := io.ReadAll(io.LimitReader(f, maxCompareFileBytes+1))
	if err != nil {
		return Data{}, fmt.Errorf("read report %q: %w", path, err)
	}
	if len(b) > maxCompareFileBytes {
		return Data{}, fmt.Errorf("report %q exceeds %d bytes", path, maxCompareFileBytes)
	}
	var d Data
	if err := json.Unmarshal(b, &d); err != nil {
		return Data{}, fmt.Errorf("decode report %q: %w", path, err)
	}
	return d, nil
}

// CompareFiles loads two JSON report files and computes their regression diff.
func CompareFiles(oldPath, newPath string, opts CompareOptions) (CompareResult, error) {
	oldData, err := LoadJSONFile(oldPath)
	if err != nil {
		return CompareResult{}, err
	}
	newData, err := LoadJSONFile(newPath)
	if err != nil {
		return CompareResult{}, err
	}
	return Compare(oldData, newData, opts), nil
}

// Compare computes deltas and evaluates regression gates between oldData and newData.
func Compare(oldData, newData Data, opts CompareOptions) CompareResult {
	res := CompareResult{
		OldGeneratedAt:   oldData.GeneratedAt,
		NewGeneratedAt:   newData.GeneratedAt,
		Target:           newData.Config.Hit,
		OldRescuedPct:    oldData.RescuedPercent,
		NewRescuedPct:    newData.RescuedPercent,
		RescuedDeltaPct:  newData.RescuedPercent - oldData.RescuedPercent,
		OldFailureRate:   oldData.FailureRate,
		NewFailureRate:   newData.FailureRate,
		FailureRateDelta: newData.FailureRate - oldData.FailureRate,
		OldP50MS:         Millis(oldData.P50),
		NewP50MS:         Millis(newData.P50),
		OldP95MS:         Millis(oldData.P95),
		NewP95MS:         Millis(newData.P95),
		P95DeltaMS:       Millis(newData.P95) - Millis(oldData.P95),
		OldP99MS:         Millis(oldData.P99),
		NewP99MS:         Millis(newData.P99),
		OldAssetP95MS:    Millis(oldData.Assets.P95),
		NewAssetP95MS:    Millis(newData.Assets.P95),
		AssetP95DeltaMS:  Millis(newData.Assets.P95) - Millis(oldData.Assets.P95),
		OldQueueHeld:     oldData.Room.Held,
		NewQueueHeld:     newData.Room.Held,
		QueueHeldDelta:   newData.Room.Held - oldData.Room.Held,
	}

	// Per-path comparison.
	oldPaths := make(map[string]PathReport, len(oldData.Paths))
	for _, p := range oldData.Paths {
		oldPaths[p.URL] = p
	}
	seen := make(map[string]bool)
	for _, np := range newData.Paths {
		seen[np.URL] = true
		op := oldPaths[np.URL]
		res.Paths = append(res.Paths, PathDiff{
			URL:         np.URL,
			OldHits:     op.Hits,
			NewHits:     np.Hits,
			OldP95MS:    Millis(op.P95),
			NewP95MS:    Millis(np.P95),
			P95DeltaMS:  Millis(np.P95) - Millis(op.P95),
			OldFailed:   op.Failed,
			NewFailed:   np.Failed,
			FailedDelta: np.Failed - op.Failed,
		})
	}
	for _, op := range oldData.Paths {
		if seen[op.URL] {
			continue
		}
		res.Paths = append(res.Paths, PathDiff{
			URL:         op.URL,
			OldHits:     op.Hits,
			OldP95MS:    Millis(op.P95),
			P95DeltaMS:  -Millis(op.P95),
			OldFailed:   op.Failed,
			FailedDelta: -op.Failed,
		})
	}
	sort.Slice(res.Paths, func(i, j int) bool {
		if res.Paths[i].P95DeltaMS != res.Paths[j].P95DeltaMS {
			return res.Paths[i].P95DeltaMS > res.Paths[j].P95DeltaMS
		}
		return res.Paths[i].URL < res.Paths[j].URL
	})

	// Compare error artifact fingerprints.
	oldFP := make(map[string]string)
	for _, a := range oldData.Forensics.Artifacts {
		oldFP[a.Fingerprint] = fmt.Sprintf("%s (%s)", a.Path, a.TriggerReason)
	}
	newFP := make(map[string]string)
	for _, a := range newData.Forensics.Artifacts {
		newFP[a.Fingerprint] = fmt.Sprintf("%s (%s)", a.Path, a.TriggerReason)
	}
	for fp, desc := range newFP {
		if _, existed := oldFP[fp]; !existed {
			res.NewFingerprints = append(res.NewFingerprints, desc)
		}
	}
	for fp, desc := range oldFP {
		if _, exists := newFP[fp]; !exists {
			res.ResolvedFingerprints = append(res.ResolvedFingerprints, desc)
		}
	}
	sort.Strings(res.NewFingerprints)
	sort.Strings(res.ResolvedFingerprints)

	// Evaluate regression thresholds.
	if opts.MaxFailureDelta >= 0 {
		passed := res.FailureRateDelta <= opts.MaxFailureDelta
		res.Gates = append(res.Gates, GateResult{
			Name:     "failure rate delta",
			Limit:    fmt.Sprintf("≤ +%.2f%%", opts.MaxFailureDelta*100),
			Observed: fmt.Sprintf("%+.2f%%", res.FailureRateDelta*100),
			Passed:   passed,
		})
	}
	if opts.MaxP95DeltaMS > 0 {
		passed := res.P95DeltaMS <= opts.MaxP95DeltaMS
		res.Gates = append(res.Gates, GateResult{
			Name:     "p95 latency delta",
			Limit:    fmt.Sprintf("≤ +%.1f ms", opts.MaxP95DeltaMS),
			Observed: fmt.Sprintf("%+.1f ms", res.P95DeltaMS),
			Passed:   passed,
		})
	}
	if opts.MaxRescuedDrop >= 0 {
		drop := -res.RescuedDeltaPct
		passed := drop <= opts.MaxRescuedDrop
		res.Gates = append(res.Gates, GateResult{
			Name:     "rescued drop",
			Limit:    fmt.Sprintf("≤ %.1f%%", opts.MaxRescuedDrop),
			Observed: fmt.Sprintf("%+.1f%%", res.RescuedDeltaPct),
			Passed:   passed,
		})
	}
	res.Regressed = anyFailed(res.Gates)
	return res
}
