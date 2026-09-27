package main

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"strings"
)

var experienceHTML = template.Must(template.New("experience").Funcs(template.FuncMap{
	"percent": func(v float64) float64 { return v * 100 },
}).Parse(`
<section id="experience">
<h2>User experience</h2>
<p>HTTP checks examine server responses. The optional Chromium cohort verifies visible content and executes JavaScript. A passing check establishes only the configured assertions, not complete visual correctness.</p>
<table><tr><th>Metric</th><th>Observed</th></tr>
<tr><td>Sessions started / not started</td><td>{{.SessionsStarted}} / {{.SessionsNotStarted}}</td></tr>
<tr><td>Sessions collected / sessions with failures</td><td>{{.Sessions}} / {{.SessionsFailed}}</td></tr>
<tr><td>Failed page visits / cancelled</td><td>{{.Failed}} / {{.Cancelled}}</td></tr>
<tr><td>Failure rate (cancelled visits excluded)</td><td>{{printf "%.2f" (percent .FailureRate)}}%</td></tr>
<tr><td>Page visits per second</td><td>{{printf "%.2f" .Throughput}}</td></tr>
<tr><td>Final-hop TTFB p95</td><td>{{.P95TTFB}}</td></tr>
<tr><td>HTML responses / checksum changes</td><td>{{.HTMLPages}} / {{.ChecksumChanges}}</td></tr>
<tr><td>Dropped session logs / trace records</td><td>{{.DroppedLifeLogs}} / {{.TraceDropped}}</td></tr></table>
<p>{{.PercentileMethod}}. Checksum changes are informational unless strict mode is enabled. Byte counts are decoded document bodies, including queue polls; redirect-body and browser asset bytes are excluded.</p>
{{if .TraceError}}<p class="badge badge-5xx">Trace error: {{.TraceError}}</p>{{end}}
{{if .GateFailures}}<h3>Failed gates</h3><ul>{{range .GateFailures}}<li>{{.}}</li>{{end}}</ul>{{else}}<p>No enabled budget was exceeded.</p>{{end}}
<h3>Exact status codes</h3><table><tr><th>Final page status</th><th>Count</th></tr>{{range $code,$count:=.StatusCodes}}<tr><td>{{$code}}</td><td>{{$count}}</td></tr>{{end}}</table>
<p>Status 0 means no HTTP response was received.</p>
<table><tr><th>HTTP responses, including redirects and queue polls</th><th>Count</th></tr>{{range $code,$count:=.ResponseCodes}}<tr><td>{{$code}}</td><td>{{$count}}</td></tr>{{end}}</table>
<h3>What failed</h3><table><tr><th>Assertion</th><th>Failures</th></tr>{{range $name,$count:=.CheckFailures}}<tr><td>{{$name}}</td><td>{{$count}}</td></tr>{{else}}<tr><td colspan="2">No failed HTTP assertions</td></tr>{{end}}</table>
<table><tr><th>Transport error category</th><th>Count</th></tr>{{range $name,$count:=.ErrorKinds}}<tr><td>{{$name}}</td><td>{{$count}}</td></tr>{{end}}</table>
{{with .Browser}}<h2>Chromium verification</h2>
<p>{{.Sessions}} additional browser sessions · {{.Visits}} visits · {{.Failed}} failed · {{.Cancelled}} cancelled. Browser requests are excluded from the HTTP cohort's counters and latency percentiles.</p>
{{if .Error}}<p class="badge badge-5xx">{{.Error}}</p>{{end}}
<p>Evidence directory: <code>{{.Output}}</code>. {{.Omitted}} older browser visits omitted here; all visits are in its JSONL trace. External requests are blocked and reported as failed dependencies. LCP and CLS are observations within the short settle window.</p>
<table><tr><th>All browser response codes (documents and resources)</th><th>Count</th></tr>{{range $code,$count:=.ResponseCodes}}<tr><td>{{$code}}</td><td>{{$count}}</td></tr>{{end}}</table>
{{range .Samples}}<details><summary>{{.Session}} · {{.Status}} · {{.URL}} · {{if .Cancelled}}cancelled{{else if .Failed}}FAILED{{else}}passed{{end}}</summary>
<p>Title: {{.Title}} · {{printf "%.1f" .DurationMS}} ms · final URL: {{.FinalURL}}</p>
<table><tr><th>Check</th><th>Passed</th><th>Detail</th></tr>{{range .Checks}}<tr><td>{{.Name}}</td><td>{{.Passed}}</td><td>{{.Detail}}</td></tr>{{end}}</table>
{{range .PageErrors}}<p>JavaScript: {{.}}</p>{{end}}{{range .ConsoleErrors}}<p>Console: {{.}}</p>{{end}}{{range .FailedResources}}<p>Resource: {{.}}</p>{{end}}
{{if .Screenshot}}<p>Failure screenshot: <code>{{.Screenshot}}</code></p>{{end}}</details>{{end}}{{end}}
<h2>HTTP session samples</h2><p>Latest 100 collected sessions, each retaining its last 100 visits. Counts above include every streamed visit. Query values, cookie values, credentials, and response bodies are not written to HTTP evidence.</p>
{{range .RetainedSessions}}<details><summary>{{.ID}} · {{.Visits}} visits · {{.Failed}} failures · {{.ExitReason}}</summary>
<p>{{.UserAgent}} · {{.Language}} · {{printf "%.1f" .DurationMS}} ms · {{.Omitted}} older visits omitted</p>
<table><tr><th># / step</th><th>URL / title</th><th>Status</th><th>Time</th><th>Result</th></tr>
{{range .Tail}}<tr><td>{{.Sequence}} {{.Step}}</td><td>{{.URL}}<br>{{.Page.Title}}</td><td>{{.Status}}</td><td>{{printf "%.1f" .DurationMS}} ms</td><td>{{if .Cancelled}}cancelled{{else if .Failed}}FAILED{{else}}passed{{end}}{{range .Page.Checks}}{{if not .Passed}}<br>{{.Name}}: {{.Detail}}{{end}}{{end}}{{if .ErrorKind}}<br>{{.ErrorKind}}{{end}}</td></tr>{{end}}</table></details>{{end}}
</section>
`))

func renderExperienceHTML(data ExperienceReport) (string, error) {
	var b bytes.Buffer
	err := experienceHTML.Execute(&b, data)
	return b.String(), err
}

func appendExperienceMarkdown(md string, data ExperienceReport) string {
	// Reuse encoding through the JSON companion for the full machine-readable
	// evidence. Markdown stays short enough to read in an email.
	var b strings.Builder
	b.WriteString(md)
	b.WriteString("\n## User experience\n\nHTTP response checks do not execute JavaScript or prove visual correctness. See the HTML and JSON reports for session traces, exact status codes, failed assertions, and optional Chromium results.\n\n")
	fmt.Fprintf(&b, "| Metric | Value |\n|---|---:|\n| Sessions observed | %d |\n| Sessions with failed visits | %d |\n| Failed visits | %d |\n| Cancelled visits | %d |\n| Failure rate | %.2f%% |\n| Visits/second | %.2f |\n| p95 final-hop TTFB | %s |\n| HTML responses | %d |\n| Checksum changes | %d |\n| Dropped session logs / trace records | %d / %d |\n\n", data.Sessions, data.SessionsFailed, data.Failed, data.Cancelled, data.FailureRate*100, data.Throughput, data.P95TTFB, data.HTMLPages, data.ChecksumChanges, data.DroppedLifeLogs, data.TraceDropped)
	b.WriteString(data.PercentileMethod + ".\n\n### Final status codes\n\n| Code | Count |\n|---|---:|\n")
	codes := make([]int, 0, len(data.StatusCodes))
	for code := range data.StatusCodes {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	for _, code := range codes {
		fmt.Fprintf(&b, "| %d | %d |\n", code, data.StatusCodes[code])
	}
	b.WriteString("\n### Failed assertions\n\n")
	names := make([]string, 0, len(data.CheckFailures))
	for name := range data.CheckFailures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "- %s: %d\n", name, data.CheckFailures[name])
	}
	for _, failure := range data.GateFailures {
		fmt.Fprintf(&b, "\n**FAILED gate:** %s\n", failure)
	}
	if browser := data.Browser; browser != nil {
		fmt.Fprintf(&b, "\n### Chromium cohort\n\n%d sessions; %d visits; %d failed; %d cancelled.\n\n%s\n", browser.Sessions, browser.Visits, browser.Failed, browser.Cancelled, browser.Error)
	}
	return b.String()
}
