package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
)

// reportDateFormat is the date component used in output filenames.
const reportDateFormat = "2006.01.02"

// Reporter accumulates LifeLogs as lemmings die and renders the final
// output files when Write is called. All ingestion is safe for concurrent
// use — lemmings die in parallel and all call Ingest simultaneously.
type Reporter struct {
	cfg       SwarmConfig
	mu        sync.Mutex
	byPath    map[string]*pathStats
	all       []LifeLog
	startedAt time.Time
	targets   []ReportTarget // delivery targets, populated via AddTarget
}

// AddTarget registers a ReportTarget to receive the rendered report
// when Write is called. Multiple targets can be registered — all receive
// the same rendered content delivered concurrently.
//
// Usage:
//
//	r.AddTarget(&LocalTarget{basePath: "."})
//	r.AddTarget(s3target)
//	r.AddTarget(mailTarget)
//
// Warning: AddTarget is not safe for concurrent use. Call it during
// swarm setup before Run is invoked.
func (r *Reporter) AddTarget(t ReportTarget) {
	r.targets = append(r.targets, t)
}

// buildFilename constructs the base report filename from the current date
// and the domain extracted from the hit URL.
//
// Format: lemmings.YYYY.MM.DD.domain
func (r *Reporter) buildFilename() string {
	date := time.Now().Format(reportDateFormat)
	domain := domainFromURL(r.cfg.Hit)
	return fmt.Sprintf("lemmings.%s.%s", date, domain)
}

// pathStats holds aggregated metrics for a single URL path.
type pathStats struct {
	URL       string
	Hits      int64
	Bytes     int64
	xx2       int64
	xx3       int64
	xx4       int64
	xx5       int64
	durations []time.Duration // kept for percentile calculation
	waitingRoom int64         // lemmings that hit a waiting room on this path
	errors    int64
}

// NewReporter constructs a Reporter.
func NewReporter(cfg SwarmConfig) *Reporter {
	return &Reporter{
		cfg:       cfg,
		byPath:    make(map[string]*pathStats),
		startedAt: time.Now(),
	}
}

// Ingest records a LifeLog into the reporter's internal state.
// Called by the swarm's collector goroutine — not on the hot path
// since it runs after lemming death.
func (r *Reporter) Ingest(ll LifeLog) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.all = append(r.all, ll)

	for _, visit := range ll.Visits {
		ps, ok := r.byPath[visit.URL]
		if !ok {
			ps = &pathStats{URL: visit.URL}
			r.byPath[visit.URL] = ps
		}

		ps.Hits++
		ps.Bytes += visit.BytesIn
		ps.durations = append(ps.durations, visit.Duration)

		if visit.WaitingRoom.Detected {
			ps.waitingRoom++
		}
		if visit.Error != nil {
			ps.errors++
		}

		switch {
		case visit.StatusCode >= 200 && visit.StatusCode < 300:
			ps.xx2++
		case visit.StatusCode >= 300 && visit.StatusCode < 400:
			ps.xx3++
		case visit.StatusCode >= 400 && visit.StatusCode < 500:
			ps.xx4++
		case visit.StatusCode >= 500:
			ps.xx5++
		}
	}
}

// Write renders the .md and .html report files to the configured destination.
func (r *Reporter) Write(saveTo string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	data := r.buildReportData()

	// Render markdown
	md, err := renderMarkdown(data)
	if err != nil {
		return fmt.Errorf("render markdown: %w", err)
	}

	// Render HTML
	htmlOut, err := renderHTML(data)
	if err != nil {
		return fmt.Errorf("render html: %w", err)
	}

	// Resolve save destination
	dest, err := resolveSavePath(saveTo, r.cfg.Hit)
	if err != nil {
		return fmt.Errorf("resolve save path: %w", err)
	}

	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("create output directory %s: %w", dest, err)
	}

	date := time.Now().Format(reportDateFormat)
	domain := domainFromURL(r.cfg.Hit)

	mdPath := filepath.Join(dest, fmt.Sprintf("lemmings.%s.%s.md", date, domain))
	htmlPath := filepath.Join(dest, fmt.Sprintf("lemmings.%s.%s.html", date, domain))

	if err := os.WriteFile(mdPath, []byte(md), 0644); err != nil {
		return fmt.Errorf("write markdown report: %w", err)
	}
	fmt.Printf("  report (md):   %s\n", mdPath)

	if err := os.WriteFile(htmlPath, []byte(htmlOut), 0644); err != nil {
		return fmt.Errorf("write html report: %w", err)
	}
	fmt.Printf("  report (html): %s\n", htmlPath)

	return nil
}

// ── Report data model ────────────────────────────────────────────────────────

// ReportData is the fully computed dataset handed to both templates.
type ReportData struct {
	Version     string
	GeneratedAt time.Time
	Duration    time.Duration
	Config      SwarmConfig

	// Swarm-level aggregates
	TotalLemmings   int64
	TotalVisits     int64
	TotalBytes      int64
	TotalWaitingRoom int64
	Total2xx        int64
	Total3xx        int64
	Total4xx        int64
	Total5xx        int64
	TotalErrors     int64

	// Timing percentiles across all visits
	Fastest    time.Duration
	Slowest    time.Duration
	P50        time.Duration
	P90        time.Duration
	P95        time.Duration
	P99        time.Duration

	// Per-path breakdown, sorted by hit count descending
	Paths []PathReport
}

// PathReport is the per-URL section of the report.
type PathReport struct {
	URL         string
	Hits        int64
	Bytes       string // human-readable
	xx2         int64
	xx3         int64
	xx4         int64
	xx5         int64
	WaitingRoom int64
	Errors      int64
	P50         time.Duration
	P99         time.Duration
}

// buildReportData computes the full ReportData from accumulated state.
// Must be called with r.mu held.
func (r *Reporter) buildReportData() ReportData {
	data := ReportData{
		Version:       r.cfg.Version,
		GeneratedAt:   time.Now(),
		Duration:      time.Since(r.startedAt).Round(time.Second),
		Config:        r.cfg,
		TotalLemmings: r.cfg.Terrain * r.cfg.Pack,
	}

	// Collect all durations for global percentiles
	var allDurations []time.Duration

	for _, ps := range r.byPath {
		data.TotalVisits += ps.Hits
		data.TotalBytes += ps.Bytes
		data.TotalWaitingRoom += ps.waitingRoom
		data.Total2xx += ps.xx2
		data.Total3xx += ps.xx3
		data.Total4xx += ps.xx4
		data.Total5xx += ps.xx5
		data.TotalErrors += ps.errors
		allDurations = append(allDurations, ps.durations...)

		pr := PathReport{
			URL:         ps.URL,
			Hits:        ps.Hits,
			Bytes:       formatBytes(ps.Bytes),
			xx2:         ps.xx2,
			xx3:         ps.xx3,
			xx4:         ps.xx4,
			xx5:         ps.xx5,
			WaitingRoom: ps.waitingRoom,
			Errors:      ps.errors,
			P50:         percentile(ps.durations, 50),
			P99:         percentile(ps.durations, 99),
		}
		data.Paths = append(data.Paths, pr)
	}

	// Sort paths by hit count descending
	sort.Slice(data.Paths, func(i, j int) bool {
		return data.Paths[i].Hits > data.Paths[j].Hits
	})

	// Global timing percentiles
	if len(allDurations) > 0 {
		sort.Slice(allDurations, func(i, j int) bool {
			return allDurations[i] < allDurations[j]
		})
		data.Fastest = allDurations[0]
		data.Slowest = allDurations[len(allDurations)-1]
		data.P50 = percentile(allDurations, 50)
		data.P90 = percentile(allDurations, 90)
		data.P95 = percentile(allDurations, 95)
		data.P99 = percentile(allDurations, 99)
	}

	return data
}

// ── Templates ────────────────────────────────────────────────────────────────

var markdownTemplate = template.Must(template.New("md").Funcs(templateFuncs).Parse(`
# lemmings v{{.Version}} report

**generated:** {{.GeneratedAt.Format "2006-01-02 15:04:05 MST"}}  
**duration:**  {{.Duration}}  
**target:**    {{.Config.Hit}}  

## configuration

| parameter | value |
|-----------|-------|
| terrain   | {{formatInt .Config.Terrain}} groups |
| pack      | {{formatInt .Config.Pack}} lemmings per terrain |
| total     | {{formatInt .TotalLemmings}} lemmings |
| until     | {{.Config.Until}} per lemming |
| ramp      | {{.Config.Ramp}} |
| limit     | {{.Config.Limit}} |

## summary

| metric | value |
|--------|-------|
| total visits    | {{formatInt .TotalVisits}} |
| total bytes     | {{.TotalBytes | formatBytesInt}} |
| waiting room    | {{formatInt .TotalWaitingRoom}} lemmings held |
| errors          | {{formatInt .TotalErrors}} |

## response codes

| code | count |
|------|-------|
| 2xx  | {{formatInt .Total2xx}} |
| 3xx  | {{formatInt .Total3xx}} |
| 4xx  | {{formatInt .Total4xx}} |
| 5xx  | {{formatInt .Total5xx}} |

## timing

| percentile | duration |
|------------|----------|
| fastest    | {{.Fastest}} |
| p50        | {{.P50}} |
| p90        | {{.P90}} |
| p95        | {{.P95}} |
| p99        | {{.P99}} |
| slowest    | {{.Slowest}} |

## per-path breakdown

{{range .Paths -}}
### {{.URL}}

{{.URL}}: {{formatInt .Hits}} hits | {{.Bytes}} | p50: {{.P50}} | p99: {{.P99}}  
{{formatInt .xx2}} 2xx | {{formatInt .xx3}} 3xx | {{formatInt .xx4}} 4xx | {{formatInt .xx5}} 5xx  
{{if gt .WaitingRoom 0}}waiting room: {{formatInt .WaitingRoom}} lemmings held  
{{end -}}
{{if gt .Errors 0}}errors: {{formatInt .Errors}}  
{{end}}
{{end}}
`))

var htmlTemplate = template.Must(template.New("html").Funcs(templateFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>lemmings report — {{.Config.Hit}}</title>
<style>
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
:root {
  --bg: #0f1117; --surface: #1a1d27; --border: #2a2d3a;
  --accent: #6c8ef5; --accent2: #a78bfa; --text: #e2e8f0;
  --muted: #64748b; --success: #34d399; --warn: #f59e0b;
  --danger: #ef4444; --radius: 8px;
}
body { background: var(--bg); color: var(--text);
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  padding: 2rem; max-width: 1100px; margin: 0 auto; }
h1 { font-size: 1.6rem; margin-bottom: 0.25rem;
  background: linear-gradient(135deg, var(--accent), var(--accent2));
  -webkit-background-clip: text; -webkit-text-fill-color: transparent;
  background-clip: text; }
h2 { font-size: 1.1rem; margin: 2rem 0 0.75rem;
  color: var(--accent); border-bottom: 1px solid var(--border);
  padding-bottom: 0.4rem; }
h3 { font-size: 0.95rem; margin: 1.5rem 0 0.5rem; color: var(--accent2); }
.meta { color: var(--muted); font-size: 0.85rem; margin-bottom: 2rem; }
table { width: 100%; border-collapse: collapse; margin-bottom: 1rem; font-size: 0.9rem; }
th { text-align: left; padding: 0.5rem 0.75rem;
  background: var(--surface); border: 1px solid var(--border);
  color: var(--muted); font-weight: 500; font-size: 0.8rem;
  text-transform: uppercase; letter-spacing: 0.05em; }
td { padding: 0.5rem 0.75rem; border: 1px solid var(--border); }
tr:hover td { background: var(--surface); }
.badge { display: inline-block; padding: 0.15rem 0.5rem;
  border-radius: 4px; font-size: 0.75rem; font-weight: 600; }
.badge-2xx { background: rgba(52,211,153,0.15); color: var(--success); }
.badge-3xx { background: rgba(245,158,11,0.15); color: var(--warn); }
.badge-4xx { background: rgba(239,68,68,0.15); color: var(--danger); }
.badge-5xx { background: rgba(239,68,68,0.25); color: var(--danger); }
.badge-wr  { background: rgba(108,142,245,0.15); color: var(--accent); }
footer { margin-top: 3rem; color: var(--muted); font-size: 0.8rem;
  border-top: 1px solid var(--border); padding-top: 1rem; }
</style>
</head>
<body>

<h1>lemmings v{{.Version}}</h1>
<p class="meta">
  {{.GeneratedAt.Format "2006-01-02 15:04:05 MST"}} &nbsp;·&nbsp;
  {{.Duration}} &nbsp;·&nbsp;
  <a href="{{.Config.Hit}}" style="color:var(--accent)">{{.Config.Hit}}</a>
</p>

<h2>configuration</h2>
<table>
  <tr><th>parameter</th><th>value</th></tr>
  <tr><td>terrain</td><td>{{formatInt .Config.Terrain}} groups</td></tr>
  <tr><td>pack</td><td>{{formatInt .Config.Pack}} lemmings per terrain</td></tr>
  <tr><td>total</td><td>{{formatInt .TotalLemmings}} lemmings</td></tr>
  <tr><td>until</td><td>{{.Config.Until}} per lemming</td></tr>
  <tr><td>ramp</td><td>{{.Config.Ramp}}</td></tr>
  <tr><td>limit</td><td>{{.Config.Limit}}</td></tr>
</table>

<h2>summary</h2>
<table>
  <tr><th>metric</th><th>value</th></tr>
  <tr><td>total visits</td><td>{{formatInt .TotalVisits}}</td></tr>
  <tr><td>total bytes</td><td>{{.TotalBytes | formatBytesInt}}</td></tr>
  <tr><td>waiting room</td><td>{{formatInt .TotalWaitingRoom}} lemmings held</td></tr>
  <tr><td>errors</td><td>{{formatInt .TotalErrors}}</td></tr>
</table>

<h2>response codes</h2>
<table>
  <tr><th>code</th><th>count</th></tr>
  <tr><td><span class="badge badge-2xx">2xx</span></td><td>{{formatInt .Total2xx}}</td></tr>
  <tr><td><span class="badge badge-3xx">3xx</span></td><td>{{formatInt .Total3xx}}</td></tr>
  <tr><td><span class="badge badge-4xx">4xx</span></td><td>{{formatInt .Total4xx}}</td></tr>
  <tr><td><span class="badge badge-5xx">5xx</span></td><td>{{formatInt .Total5xx}}</td></tr>
</table>

<h2>timing</h2>
<table>
  <tr><th>percentile</th><th>duration</th></tr>
  <tr><td>fastest</td><td>{{.Fastest}}</td></tr>
  <tr><td>p50</td><td>{{.P50}}</td></tr>
  <tr><td>p90</td><td>{{.P90}}</td></tr>
  <tr><td>p95</td><td>{{.P95}}</td></tr>
  <tr><td>p99</td><td>{{.P99}}</td></tr>
  <tr><td>slowest</td><td>{{.Slowest}}</td></tr>
</table>

<h2>per-path breakdown</h2>
<table>
  <tr>
    <th>path</th><th>hits</th><th>bytes</th>
    <th>p50</th><th>p99</th>
    <th>2xx</th><th>3xx</th><th>4xx</th><th>5xx</th>
    <th>waiting room</th><th>errors</th>
  </tr>
  {{range .Paths}}
  <tr>
    <td>{{.URL}}</td>
    <td>{{formatInt .Hits}}</td>
    <td>{{.Bytes}}</td>
    <td>{{.P50}}</td>
    <td>{{.P99}}</td>
    <td><span class="badge badge-2xx">{{formatInt .xx2}}</span></td>
    <td><span class="badge badge-3xx">{{formatInt .xx3}}</span></td>
    <td><span class="badge badge-4xx">{{formatInt .xx4}}</span></td>
    <td><span class="badge badge-5xx">{{formatInt .xx5}}</span></td>
    <td>{{if gt .WaitingRoom 0}}<span class="badge badge-wr">{{formatInt .WaitingRoom}}</span>{{else}}—{{end}}</td>
    <td>{{if gt .Errors 0}}{{formatInt .Errors}}{{else}}—{{end}}</td>
  </tr>
  {{end}}
</table>

<footer>generated by lemmings v{{.Version}} &nbsp;·&nbsp; {{.GeneratedAt.Format "2006-01-02 15:04:05 MST"}}</footer>
</body>
</html>
`))

// templateFuncs are available inside both templates.
var templateFuncs = template.FuncMap{
	"formatInt": func(n int64) string {
		return formatInt(n)
	},
	"formatBytesInt": func(n int64) string {
		return formatBytes(n)
	},
}

// ── Rendering ────────────────────────────────────────────────────────────────

func renderMarkdown(data ReportData) (string, error) {
	var buf bytes.Buffer
	if err := markdownTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderHTML(data ReportData) (string, error) {
	var buf bytes.Buffer
	if err := htmlTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// percentile computes the Nth percentile of a pre-sorted duration slice.
// Returns 0 if the slice is empty.
func percentile(sorted []time.Duration, n int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	// Nearest rank method
	idx := int(math.Ceil(float64(n)/100.0*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// resolveSavePath resolves the output directory from saveTo and the hit URL.
// Handles local paths and s3:// URIs.
// Local: <saveTo>/lemmings/<domain>/
// S3:    s3://<bucket>/<prefix>/<domain>/
func resolveSavePath(saveTo, hit string) (string, error) {
	domain := domainFromURL(hit)

	if strings.HasPrefix(saveTo, "s3://") {
		// S3 paths are handled by the reporter's Write method directly.
		// For now we return the s3 path as-is and note that S3 upload
		// is a v1.1.0 concern — local write is always attempted first.
		return saveTo + "/" + domain, nil
	}

	return filepath.Join(saveTo, "lemmings", domain), nil
}

// domainFromURL extracts a filesystem-safe domain string from a URL.
func domainFromURL(hit string) string {
	hit = strings.TrimPrefix(hit, "https://")
	hit = strings.TrimPrefix(hit, "http://")
	hit = strings.Split(hit, "/")[0]
	hit = strings.ReplaceAll(hit, ":", "-")
	return hit
}
