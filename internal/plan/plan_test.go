package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const testOrigin = "http://127.0.0.1:8080/"

func TestParsePlanYAML_SinglePlanFullSchema(t *testing.T) {
	t.Setenv("TEST_SESSION_TOKEN", "tok-abc-123")
	t.Setenv("TEST_API_KEY", "key-xyz-999")
	t.Setenv("TEST_PASSWORD", "s3cr3t!")

	doc := `---
name: shopper
weight: 3
stop-on-fail: true
loop: true
plan:
  - name: landing
    path: /
    chill: 500ms
    want-status: 200
    want-content-type: text/html
    want-title: Neon
    want-selectors: [nav, "#app", .ready]
    want-body:
      - contains: Welcome
        not-contains: Fatal
  - path: /login
    method: post
    chill: 0s
    follow-redirects: false
    stop-on-fail: false
    with-cookies:
      - name: pre_session
        value: "${TEST_SESSION_TOKEN}"
      - key: numeric_flag
        value: 1
      - name: bool_flag
        value: true
      - name: literal_dollar
        value: "$$100-$${NOT_A_VAR}"
    with-headers:
      - key: X-API-Key
        value: "${TEST_API_KEY}"
      - name: X-Bool
        value: false
    with-body:
      - key: user
        value: alice
      - name: pass
        value: "${TEST_PASSWORD}"
      - key: qty
        value: 42
    want-status: "302, 303"
    want-headers:
      - key: Location
        begins-with: /dashboard
      - name: X-Mode
        is-one: "fast, slow"
    want-cookies:
      - name: session
        begins-with: "s_"
      - key: old_session
        absent: true
  - name: api-raw
    path: /api/items
    method: PATCH
    with-headers:
      - key: Content-Type
        value: application/json
    body: '{"token":"${TEST_API_KEY}","price":"$$5"}'
    want-status:
      - 200
      - "201-204"
    want-headers:
      - key: X-Variant
        is-one: [alpha, beta]
...
`
	plans, err := ParsePlanYAML(strings.NewReader(doc), "ignored-default", testOrigin, DefaultMaxPlanItems)
	if err != nil {
		t.Fatalf("ParsePlanYAML failed: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	p := plans[0]
	if p.Name != "shopper" || p.Weight != 3 || !p.StopOnFail || !p.Loop {
		t.Fatalf("unexpected plan metadata: %+v", p)
	}
	if !p.HasNonGET() {
		t.Error("expected HasNonGET() == true")
	}
	if len(p.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(p.Items))
	}

	// Step 1 checks
	s0 := p.Items[0]
	if s0.Name != "landing" || s0.EffectiveMethod() != http.MethodGet {
		t.Errorf("step 0 mismatch: %+v", s0)
	}
	if s0.Chill == nil || *s0.Chill != 500*time.Millisecond {
		t.Errorf("step 0 chill mismatch: %v", s0.Chill)
	}
	if !s0.ShouldFollowRedirects() || !s0.ShouldStopOnFail(p.StopOnFail) || s0.HasRequestBody() {
		t.Errorf("step 0 helper mismatch: follow=%v stop=%v body=%v", s0.ShouldFollowRedirects(), s0.ShouldStopOnFail(p.StopOnFail), s0.HasRequestBody())
	}

	// Step 2 checks
	s1 := p.Items[1]
	if s1.Name != "step-2" {
		t.Errorf("expected default step name step-2, got %q", s1.Name)
	}
	if s1.Method != http.MethodPost || s1.EffectiveMethod() != http.MethodPost {
		t.Errorf("expected normalized POST, got %q", s1.Method)
	}
	if s1.Chill == nil || *s1.Chill != 0 {
		t.Errorf("expected explicit 0s chill, got %v", s1.Chill)
	}
	if s1.ShouldFollowRedirects() {
		t.Error("expected ShouldFollowRedirects() == false")
	}
	if s1.ShouldStopOnFail(p.StopOnFail) {
		t.Error("expected step-level stop-on-fail=false to override plan default")
	}
	if !s1.HasRequestBody() {
		t.Error("expected HasRequestBody() == true for with-body")
	}
	wantCookies := []PlanCookie{
		{Name: "pre_session", Value: "tok-abc-123"},
		{Name: "numeric_flag", Value: "1"},
		{Name: "bool_flag", Value: "true"},
		{Name: "literal_dollar", Value: "$100-${NOT_A_VAR}"},
	}
	if !reflect.DeepEqual(s1.WithCookies, wantCookies) {
		t.Errorf("WithCookies mismatch:\ngot:  %+v\nwant: %+v", s1.WithCookies, wantCookies)
	}
	wantHeaders := []PlanHeader{
		{Key: "X-API-Key", Value: "key-xyz-999"},
		{Key: "X-Bool", Value: "false"},
	}
	if !reflect.DeepEqual(s1.WithHeaders, wantHeaders) {
		t.Errorf("WithHeaders mismatch:\ngot:  %+v\nwant: %+v", s1.WithHeaders, wantHeaders)
	}
	wantBody := []PlanBody{
		{Key: "user", Value: "alice"},
		{Key: "pass", Value: "s3cr3t!"},
		{Key: "qty", Value: "42"},
	}
	if !reflect.DeepEqual(s1.WithBody, wantBody) {
		t.Errorf("WithBody mismatch:\ngot:  %+v\nwant: %+v", s1.WithBody, wantBody)
	}
	if !reflect.DeepEqual(s1.WantHeaders[1].IsOne, StringList{"fast", "slow"}) {
		t.Errorf("WantHeaders IsOne mismatch: %+v", s1.WantHeaders[1].IsOne)
	}

	// Step 3 checks
	s2 := p.Items[2]
	if s2.Method != http.MethodPatch || !s2.HasRequestBody() {
		t.Errorf("step 2 mismatch: method=%q hasBody=%v", s2.Method, s2.HasRequestBody())
	}
	if s2.Body != `{"token":"key-xyz-999","price":"$5"}` {
		t.Errorf("step 2 Body mismatch: %q", s2.Body)
	}
	if s2.Chill != nil {
		t.Errorf("expected nil chill when omitted, got %v", s2.Chill)
	}
	if !reflect.DeepEqual(s2.WantHeaders[0].IsOne, StringList{"alpha", "beta"}) {
		t.Errorf("step 2 IsOne mismatch: %+v", s2.WantHeaders[0].IsOne)
	}

	// SEC-10: marshaling PlanItem to JSON must never emit sensitive with-*/body/cookie matcher values.
	rawJSON, err := json.Marshal(s1)
	if err != nil {
		t.Fatalf("json.Marshal(PlanItem): %v", err)
	}
	for _, secret := range []string{"tok-abc-123", "key-xyz-999", "s3cr3t!", "with-cookies", "with-headers", "with-body"} {
		if strings.Contains(string(rawJSON), secret) {
			t.Errorf("SEC-10 violation: JSON marshaled PlanItem leaked %q: %s", secret, rawJSON)
		}
	}
}

func TestParsePlanYAML_DefaultNameAndWeightAndAllowedMethods(t *testing.T) {
	doc := `
plan:
  - path: /get
    method: get
  - path: /post
    method: post
  - path: /put
    method: put
  - path: /patch
    method: patch
  - path: /del
    method: delete
  - path: /head
    method: head
  - path: /opt
    method: options
`
	plans, err := ParsePlanYAML(strings.NewReader(doc), "checkout-flow", testOrigin, DefaultMaxPlanItems)
	if err != nil {
		t.Fatalf("ParsePlanYAML: %v", err)
	}
	p := plans[0]
	if p.Name != "checkout-flow" || p.Weight != 1 {
		t.Fatalf("expected default name=checkout-flow weight=1, got %+v", p)
	}
	wantMethods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	for i, want := range wantMethods {
		if p.Items[i].Method != want {
			t.Errorf("item[%d].Method = %q, want %q", i, p.Items[i].Method, want)
		}
	}

	// GET-only plan HasNonGET == false
	getOnly, err := ParsePlanYAML(strings.NewReader("plan:\n  - path: /\n"), "get-only", testOrigin, DefaultMaxPlanItems)
	if err != nil {
		t.Fatalf("ParsePlanYAML get-only: %v", err)
	}
	if getOnly[0].HasNonGET() {
		t.Error("expected GET-only plan HasNonGET() == false")
	}
}

func TestParsePlanYAML_MultiPersonaBundle(t *testing.T) {
	doc := `
plans:
  - name: browser
    weight: 5
    plan:
      - path: /
  - name: buyer
    weight: 2
    stop-on-fail: true
    plan:
      - path: /cart
        method: POST
        body: "item=1"
  - weight: 0
    plan:
      - path: /about
`
	plans, err := ParsePlanYAML(strings.NewReader(doc), "bundle", testOrigin, DefaultMaxPlanItems)
	if err != nil {
		t.Fatalf("ParsePlanYAML bundle: %v", err)
	}
	if len(plans) != 3 {
		t.Fatalf("expected 3 plans, got %d", len(plans))
	}
	if plans[0].Name != "browser" || plans[0].Weight != 5 {
		t.Errorf("plan 0 mismatch: %+v", plans[0])
	}
	if plans[1].Name != "buyer" || plans[1].Weight != 2 || !plans[1].StopOnFail {
		t.Errorf("plan 1 mismatch: %+v", plans[1])
	}
	if plans[2].Name != "bundle-3" || plans[2].Weight != 1 {
		t.Errorf("plan 2 mismatch (default name + weight 0->1): %+v", plans[2])
	}
}

func TestParsePlanYAML_ValidationErrors(t *testing.T) {
	t.Setenv("BAD_CRLF_ENV", "ok\r\nX-Injected: evil")

	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "empty doc",
			yaml:    "",
			wantSub: "empty",
		},
		{
			name:    "multi-document YAML rejected",
			yaml:    "plan:\n  - path: /\n---\nplan:\n  - path: /b\n",
			wantSub: "single YAML document",
		},
		{
			name:    "unknown top-level field",
			yaml:    "unknown_field: 1\nplan:\n  - path: /\n",
			wantSub: "unknown_field",
		},
		{
			name:    "unknown step field",
			yaml:    "plan:\n  - path: /\n    bogus: true\n",
			wantSub: "bogus",
		},
		{
			name:    "unknown with-cookies field",
			yaml:    "plan:\n  - path: /\n    with-cookies:\n      - name: a\n        value: b\n        extra: c\n",
			wantSub: "unknown field",
		},
		{
			name:    "unknown want-headers field",
			yaml:    "plan:\n  - path: /\n    want-headers:\n      - key: Server\n        bogus: x\n",
			wantSub: "unknown field",
		},
		{
			name:    "unknown want-cookies field",
			yaml:    "plan:\n  - path: /\n    want-cookies:\n      - name: sid\n        bogus: x\n",
			wantSub: "unknown field",
		},
		{
			name:    "unknown want-body field",
			yaml:    "plan:\n  - path: /\n    want-body:\n      - bogus: x\n",
			wantSub: "bogus",
		},
		{
			name:    "both plan and plans",
			yaml:    "plan:\n  - path: /\nplans:\n  - name: a\n    plan:\n      - path: /\n",
			wantSub: "both top-level",
		},
		{
			name:    "neither plan nor plans",
			yaml:    "name: empty\n",
			wantSub: "must specify top-level",
		},
		{
			name:    "top-level metadata with plans bundle",
			yaml:    "weight: 2\nplans:\n  - name: a\n    plan:\n      - path: /\n",
			wantSub: "cannot be set alongside",
		},
		{
			name:    "empty plans list",
			yaml:    "plans: []\n",
			wantSub: "at least 1 plan",
		},
		{
			name:    "empty plan items",
			yaml:    "plan: []\n",
			wantSub: "at least 1 plan item",
		},
		{
			name:    "negative weight",
			yaml:    "weight: -1\nplan:\n  - path: /\n",
			wantSub: "weight must be >= 0",
		},
		{
			name:    "off-origin path",
			yaml:    "plan:\n  - path: https://evil.test/steal\n",
			wantSub: "must stay on",
		},
		{
			name:    "empty path",
			yaml:    "plan:\n  - name: missing\n",
			wantSub: "must stay on",
		},
		{
			name:    "unsupported HTTP method",
			yaml:    "plan:\n  - path: /\n    method: TRACE\n",
			wantSub: "unsupported HTTP method",
		},
		{
			name:    "negative chill",
			yaml:    "plan:\n  - path: /\n    chill: -5s\n",
			wantSub: "must not be negative",
		},
		{
			name:    "unitless chill integer",
			yaml:    "plan:\n  - path: /\n    chill: 30\n",
			wantSub: "invalid chill duration",
		},
		{
			name:    "both name and key in with-cookies",
			yaml:    "plan:\n  - path: /\n    with-cookies:\n      - name: a\n        key: b\n        value: c\n",
			wantSub: "cannot set both",
		},
		{
			name:    "both key and name in with-headers",
			yaml:    "plan:\n  - path: /\n    with-headers:\n      - key: X-A\n        name: X-B\n        value: c\n",
			wantSub: "cannot set both",
		},
		{
			name:    "both key and name in with-body",
			yaml:    "plan:\n  - path: /\n    with-body:\n      - key: a\n        name: b\n        value: c\n",
			wantSub: "cannot set both",
		},
		{
			name:    "both key and name in want-headers",
			yaml:    "plan:\n  - path: /\n    want-headers:\n      - key: X-A\n        name: X-B\n",
			wantSub: "cannot set both",
		},
		{
			name:    "both name and key in want-cookies",
			yaml:    "plan:\n  - path: /\n    want-cookies:\n      - name: a\n        key: b\n",
			wantSub: "cannot set both",
		},
		{
			name:    "both value and is in want-cookies",
			yaml:    "plan:\n  - path: /\n    want-cookies:\n      - name: a\n        value: x\n        is: y\n",
			wantSub: "cannot set both",
		},
		{
			name:    "absent true with matcher in want-cookies",
			yaml:    "plan:\n  - path: /\n    want-cookies:\n      - name: a\n        absent: true\n        contains: x\n",
			wantSub: "absent: true",
		},
		{
			name:    "with-body and body mutually exclusive",
			yaml:    "plan:\n  - path: /\n    method: POST\n    with-body:\n      - key: a\n        value: 1\n    body: raw\n",
			wantSub: "mutually exclusive",
		},
		{
			name:    "Host header rejected",
			yaml:    "plan:\n  - path: /\n    with-headers:\n      - key: hOsT\n        value: evil.test\n",
			wantSub: "Host",
		},
		{
			name:    "CR/LF in header value",
			yaml:    "plan:\n  - path: /\n    with-headers:\n      - key: X-Test\n        value: \"a\\r\\nb\"\n",
			wantSub: "CR, LF, or NUL",
		},
		{
			name:    "CR/LF in header value via env expansion",
			yaml:    "plan:\n  - path: /\n    with-headers:\n      - key: X-Test\n        value: \"${BAD_CRLF_ENV}\"\n",
			wantSub: "CR, LF, or NUL",
		},
		{
			name:    "NUL in cookie name",
			yaml:    "plan:\n  - path: /\n    with-cookies:\n      - name: \"sid\\x00bad\"\n        value: ok\n",
			wantSub: "CR, LF, or NUL",
		},
		{
			name:    "empty cookie name",
			yaml:    "plan:\n  - path: /\n    with-cookies:\n      - name: \"\"\n        value: x\n",
			wantSub: "name must not be empty",
		},
		{
			name:    "empty header key",
			yaml:    "plan:\n  - path: /\n    with-headers:\n      - key: \"\"\n        value: x\n",
			wantSub: "key must not be empty",
		},
		{
			name:    "empty with-body key",
			yaml:    "plan:\n  - path: /\n    with-body:\n      - key: \"\"\n        value: x\n",
			wantSub: "key must not be empty",
		},
		{
			name:    "empty want-body entry",
			yaml:    "plan:\n  - path: /\n    want-body:\n      - {}\n",
			wantSub: "contains or not-contains",
		},
		{
			name:    "invalid selector",
			yaml:    "plan:\n  - path: /\n    want-selectors: [\"div > span\"]\n",
			wantSub: "must be a tag, #id or .class",
		},
		{
			name:    "unset env var fails closed naming var",
			yaml:    "plan:\n  - path: /\n    with-headers:\n      - key: Authorization\n        value: \"Bearer ${DEFINITELY_UNSET_LEMMINGS_VAR}\"\n",
			wantSub: "DEFINITELY_UNSET_LEMMINGS_VAR",
		},
		{
			name:    "duplicate plan names in bundle",
			yaml:    "plans:\n  - name: dup\n    plan:\n      - path: /\n  - name: dup\n    plan:\n      - path: /b\n",
			wantSub: "duplicate plan name",
		},
		{
			name:    "invalid status range min > max",
			yaml:    "plan:\n  - path: /\n    want-status: \"299-200\"\n",
			wantSub: "min 299 > max 200",
		},
		{
			name:    "status code out of range",
			yaml:    "plan:\n  - path: /\n    want-status: 600\n",
			wantSub: "out of range",
		},
		{
			name:    "empty want-status list",
			yaml:    "plan:\n  - path: /\n    want-status: []\n",
			wantSub: "must not be empty",
		},
		{
			name:    "non-scalar pair value",
			yaml:    "plan:\n  - path: /\n    with-body:\n      - key: a\n        value: [1, 2]\n",
			wantSub: "must be a scalar",
		},
		{
			name:    "empty is-one",
			yaml:    "plan:\n  - path: /\n    want-headers:\n      - key: X-Mode\n        is-one: []\n",
			wantSub: "must not be empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlanYAML(strings.NewReader(tc.yaml), "test", testOrigin, DefaultMaxPlanItems)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestNameValueAndKeyValueDirectUnmarshal(t *testing.T) {
	var nv NameValue
	if err := yaml.Unmarshal([]byte("key: sid\nvalue: 123\n"), &nv); err != nil {
		t.Fatalf("NameValue alias unmarshal: %v", err)
	}
	if nv.Name != "sid" || nv.Value != "123" {
		t.Errorf("unexpected NameValue: %+v", nv)
	}
	if err := yaml.Unmarshal([]byte("name: a\nkey: b\nvalue: c\n"), &nv); err == nil {
		t.Error("expected NameValue with both name and key to fail")
	}

	var kv KeyValue
	if err := yaml.Unmarshal([]byte("name: X-Flag\nvalue: true\n"), &kv); err != nil {
		t.Fatalf("KeyValue alias unmarshal: %v", err)
	}
	if kv.Key != "X-Flag" || kv.Value != "true" {
		t.Errorf("unexpected KeyValue: %+v", kv)
	}
	if err := yaml.Unmarshal([]byte("key: a\nname: b\nvalue: c\n"), &kv); err == nil {
		t.Error("expected KeyValue with both key and name to fail")
	}

	// Unquoted null decodes to empty string; quoted "null" stays "null".
	var nvNull NameValue
	if err := yaml.Unmarshal([]byte("name: empty\nvalue: null\n"), &nvNull); err != nil {
		t.Fatalf("NameValue null: %v", err)
	}
	if nvNull.Value != "" {
		t.Errorf("expected empty string for null, got %q", nvNull.Value)
	}
	if err := yaml.Unmarshal([]byte("name: lit\nvalue: \"null\"\n"), &nvNull); err != nil {
		t.Fatalf("NameValue quoted null: %v", err)
	}
	if nvNull.Value != "null" {
		t.Errorf("expected literal \"null\", got %q", nvNull.Value)
	}
}

func TestStatusSpec_ParsingAndMethods(t *testing.T) {
	// Empty spec = 200..299
	var empty StatusSpec
	if !empty.IsEmpty() || !empty.Allows2xx() || empty.Exclusively3xx() {
		t.Fatalf("empty StatusSpec flags mismatch: empty=%v 2xx=%v 3xx=%v", empty.IsEmpty(), empty.Allows2xx(), empty.Exclusively3xx())
	}
	if !empty.Matches(200) || !empty.Matches(299) || empty.Matches(199) || empty.Matches(300) {
		t.Error("empty StatusSpec Matches mismatch")
	}

	// Scalar int, strings, ranges, classes, sequences
	cases := []struct {
		yaml    string
		match   []int
		noMatch []int
		allow2  bool
		only3   bool
	}{
		{"200", []int{200}, []int{201, 302}, true, false},
		{`"200, 201"`, []int{200, 201}, []int{202, 404}, true, false},
		{`"2xx"`, []int{200, 204, 299}, []int{199, 300}, true, false},
		{`"3xx"`, []int{301, 302, 307}, []int{200, 400}, false, true},
		{`"301, 302-308"`, []int{301, 302, 308}, []int{300, 309, 200}, false, true},
		{`"200, 302"`, []int{200, 302}, []int{201, 404}, true, false},
		{`[200, "404", "500-503"]`, []int{200, 404, 500, 503}, []int{201, 504}, true, false},
		{`["301", 302]`, []int{301, 302}, []int{200, 303}, false, true},
		{`"404"`, []int{404}, []int{200}, false, false},
	}

	for _, tc := range cases {
		var spec StatusSpec
		if err := yaml.Unmarshal([]byte(tc.yaml), &spec); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.yaml, err)
		}
		if spec.IsEmpty() {
			t.Errorf("%s should not be empty", tc.yaml)
		}
		if spec.Allows2xx() != tc.allow2 {
			t.Errorf("%s Allows2xx() = %v, want %v", tc.yaml, spec.Allows2xx(), tc.allow2)
		}
		if spec.Exclusively3xx() != tc.only3 {
			t.Errorf("%s Exclusively3xx() = %v, want %v", tc.yaml, spec.Exclusively3xx(), tc.only3)
		}
		for _, c := range tc.match {
			if !spec.Matches(c) {
				t.Errorf("%s should match %d", tc.yaml, c)
			}
		}
		for _, c := range tc.noMatch {
			if spec.Matches(c) {
				t.Errorf("%s should not match %d", tc.yaml, c)
			}
		}
	}

	// ShouldFollowRedirects auto-defaults to false on exclusively 3xx.
	redirItem := PlanItem{Path: "/old", WantStatus: StatusSpec{{Min: 301, Max: 302}}}
	if redirItem.ShouldFollowRedirects() {
		t.Error("exclusively 3xx step should default ShouldFollowRedirects() to false")
	}
	forceTrue := true
	redirItem.FollowRedirects = &forceTrue
	if !redirItem.ShouldFollowRedirects() {
		t.Error("explicit follow-redirects: true should override 3xx default")
	}
	mixedItem := PlanItem{Path: "/maybe", WantStatus: StatusSpec{{Min: 200, Max: 200}, {Min: 302, Max: 302}}}
	if !mixedItem.ShouldFollowRedirects() {
		t.Error("mixed 200+302 step should default ShouldFollowRedirects() to true")
	}
}

func TestExpandEnvStrict(t *testing.T) {
	t.Setenv("FOO", "bar")
	t.Setenv("EMPTY_VAR", "")
	t.Setenv("DOLLAR_VAL", "$100$${FOO}")

	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"${FOO}", "bar"},
		{"prefix-${FOO}-suffix", "prefix-bar-suffix"},
		{"$${FOO}", "${FOO}"},
		{"$$${FOO}", "$bar"},
		{"$$$${FOO}", "$${FOO}"},
		{"price $$50", "price $50"},
		{"single $5", "single $5"},
		{"${EMPTY_VAR}", ""},
		{"${DOLLAR_VAL}", "$100$${FOO}"}, // substituted values are NOT re-expanded
		{"${123}", "${123}"},
		{"${bad-name}", "${bad-name}"},
		{"${unclosed", "${unclosed"},
	}
	for _, tc := range cases {
		got, err := expandEnvStrict(tc.in)
		if err != nil {
			t.Errorf("expandEnvStrict(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("expandEnvStrict(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := expandEnvStrict("Bearer ${MISSING_SECRET_XYZ}"); err == nil || !strings.Contains(err.Error(), "MISSING_SECRET_XYZ") {
		t.Errorf("expected error naming MISSING_SECRET_XYZ, got %v", err)
	}
}

func TestLoadSuite_FilesDirsWeightsLimits(t *testing.T) {
	dir := t.TempDir()
	plansDir := filepath.Join(dir, "personas")
	if err := os.MkdirAll(filepath.Join(plansDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Lexicographically ordered YAML files plus ignored non-YAML and nested YAML.
	writeTestFile(t, filepath.Join(plansDir, "01-browse.yaml"), "weight: 3\nplan:\n  - path: /\n  - path: /games\n")
	writeTestFile(t, filepath.Join(plansDir, "02-buy.yml"), "name: buyer\nweight: 1\nplan:\n  - path: /buy\n    method: POST\n")
	writeTestFile(t, filepath.Join(plansDir, "README.md"), "# ignored")
	writeTestFile(t, filepath.Join(plansDir, "nested", "ignored.yaml"), "plan:\n  - path: /nested\n")

	extraFile := filepath.Join(dir, "extra.yaml")
	writeTestFile(t, extraFile, "name: extra\nweight: 2\nplan:\n  - path: /extra\n")

	suite, err := LoadSuite(plansDir+","+extraFile, testOrigin, DefaultMaxPlanItems)
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	if len(suite.Plans) != 3 {
		t.Fatalf("expected 3 plans, got %d (%v)", len(suite.Plans), suite.Names())
	}
	wantNames := []string{"01-browse", "buyer", "extra"}
	if !reflect.DeepEqual(suite.Names(), wantNames) {
		t.Errorf("suite.Names() = %v, want %v", suite.Names(), wantNames)
	}
	if suite.PrimaryName() != "01-browse" {
		t.Errorf("suite.PrimaryName() = %q, want 01-browse", suite.PrimaryName())
	}
	if suite.TotalWeight != 6 || suite.TotalSteps != 4 {
		t.Errorf("TotalWeight=%d TotalSteps=%d, want 6 and 4", suite.TotalWeight, suite.TotalSteps)
	}
	if !suite.HasNonGET() {
		t.Error("expected suite.HasNonGET() == true")
	}

	// Deterministic weight-proportional Pick across 12 cohort indices (weights 3, 1, 2 -> total 6).
	counts := map[string]int{}
	for i := int64(0); i < 12; i++ {
		picked := suite.Pick(i)
		if picked == nil {
			t.Fatalf("Pick(%d) returned nil", i)
		}
		counts[picked.Name]++
	}
	if counts["01-browse"] != 6 || counts["buyer"] != 2 || counts["extra"] != 4 {
		t.Errorf("weighted Pick counts = %v, want 01-browse:6 buyer:2 extra:4", counts)
	}
	if suite.Pick(-1) == nil {
		t.Error("Pick(-1) should wrap safely without panicking")
	}

	// Empty directory must fail.
	emptyDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuite(emptyDir, testOrigin, DefaultMaxPlanItems); err == nil {
		t.Error("expected empty plan directory to fail")
	}

	// Max items error message exact format.
	_, err = LoadSuite(filepath.Join(plansDir, "01-browse.yaml"), testOrigin, 1)
	wantMsg := `cannot have more than 1 plan items in "01-browse" — have 2 (override with -max-plan-items)`
	if err == nil || !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("max-items error = %v, want substring %q", err, wantMsg)
	}

	// Duplicate plan name across files must fail.
	dupFile := filepath.Join(dir, "dup.yaml")
	writeTestFile(t, dupFile, "name: buyer\nplan:\n  - path: /dup\n")
	if _, err := LoadSuite(plansDir+","+dupFile, testOrigin, DefaultMaxPlanItems); err == nil || !strings.Contains(err.Error(), "duplicate plan name") {
		t.Errorf("expected duplicate plan name error, got %v", err)
	}

	// MaxSuitePlans (> 100) must fail.
	var sb strings.Builder
	sb.WriteString("plans:\n")
	for i := range MaxSuitePlans + 1 {
		fmt.Fprintf(&sb, "  - name: p-%d\n    plan:\n      - path: /p/%d\n", i, i)
	}
	if _, err := ParsePlanYAML(strings.NewReader(sb.String()), "too-many", testOrigin, DefaultMaxPlanItems); err == nil {
		t.Error("expected > MaxSuitePlans to fail")
	}

	// planReadLimit scaling check.
	if got := planReadLimit(10); got != minPlanReadBytes {
		t.Errorf("planReadLimit(10) = %d, want %d", got, minPlanReadBytes)
	}
	if got := planReadLimit(1000); got != 1000*4096 {
		t.Errorf("planReadLimit(1000) = %d, want %d", got, 1000*4096)
	}
	if got := planReadLimit(1_000_000); got != maxPlanReadBytes {
		t.Errorf("planReadLimit(1_000_000) = %d, want %d", got, maxPlanReadBytes)
	}
}

func TestInspectResponse_HTMLHeadersCookiesAndRedaction(t *testing.T) {
	base := testOrigin
	htmlDoc := []byte(`<!doctype html><html><head><title>Neon Arcade</title></head>
		<body><nav><a href="/games">Games</a><a href="/logout">Sign out</a></nav>
		<main id="app" class="ready"><h1>Insert coin</h1></main></body></html>`)

	step := &PlanItem{
		Name:            "home",
		Path:            "/",
		Method:          http.MethodGet,
		WantStatus:      StatusSpec{{Min: 200, Max: 200}},
		WantContentType: "text/html",
		WantTitle:       "Neon",
		WantBody: []ExpectBody{
			{Contains: "Insert coin", NotContains: "Something went wrong"},
		},
		WantSelectors: []string{"nav", "#app", ".ready"},
		WantHeaders: []ExpectHeader{
			{Key: "x-request-id"},
			{Key: "Cache-Control", Contains: "private", NotContains: "public", BeginsWith: "private", EndsWith: "max-age=60"},
			{Key: "X-Mode", IsOne: StringList{"fast", "turbo"}},
			{Key: "X-Exact", Is: "ok"},
			{Key: "X-Absent-Safe", NotContains: "forbidden"},
			{Key: "Set-Cookie", Contains: "session="},
			{Key: "X-Custom-Token", BeginsWith: "tok_"},
		},
		WantCookies: []ExpectCookie{
			{Name: "session", BeginsWith: "s_", EndsWith: "_end", Contains: "mid", NotContains: "bad"},
			{Name: "theme", Value: "dark"},
			{Name: "tier", IsOne: StringList{"gold", "silver"}},
			{Name: "present_only"},
			{Name: "deleted_cookie", Absent: true},
		},
	}

	hdrs := http.Header{}
	hdrs.Add("X-Request-Id", "req-123")
	hdrs.Add("Cache-Control", "no-transform")
	hdrs.Add("Cache-Control", "private, max-age=60")
	hdrs.Add("X-Mode", "turbo")
	hdrs.Add("X-Exact", "ok")
	hdrs.Add("Set-Cookie", "session=s_mid_supersecret_end; HttpOnly")
	hdrs.Add("X-Custom-Token", "tok_verysecretvalue999")

	cookies := []*http.Cookie{
		{Name: "session", Value: "s_mid_supersecret_end"},
		{Name: "theme", Value: "dark"},
		{Name: "tier", Value: "gold"},
		{Name: "present_only", Value: "raw-present-secret"},
	}

	page, links := InspectResponse(ResponseInput{
		Method:      http.MethodGet,
		Body:        htmlDoc,
		ContentType: "text/html; charset=utf-8",
		Status:      200,
		Headers:     hdrs,
		Cookies:     cookies,
		Step:        step,
		Base:        base,
	})
	if page.FailedChecks() != 0 {
		t.Fatalf("expected all checks to pass, checks: %+v", page.Checks)
	}
	if page.Title != "Neon Arcade" {
		t.Errorf("unexpected title: %q", page.Title)
	}
	for _, link := range links {
		if strings.Contains(link, "logout") {
			t.Errorf("logout link should have been filtered: %q", link)
		}
	}

	// SEC-10: verify no sensitive header value or raw cookie value appears in any CheckResult.Detail.
	for _, c := range page.Checks {
		for _, leak := range []string{"s_mid_supersecret_end", "tok_verysecretvalue999", "raw-present-secret", "dark", "gold"} {
			if strings.Contains(c.Detail, leak) {
				t.Errorf("SEC-10 violation in check %q detail %q (leaked %q)", c.Name, c.Detail, leak)
			}
		}
	}

	// Also verify SEC-10 on FAILED sensitive header & cookie checks.
	failStep := &PlanItem{
		Name: "fail-sec",
		Path: "/",
		WantHeaders: []ExpectHeader{
			{Key: "Authorization", Is: "Bearer wrong"},
			{Key: "X-Api-Key", Contains: "wrong"},
			{Key: "PHPSESSID", Is: "wrong"},
			{Key: "X-User-Secret", Is: "wrong"},
			{Key: "X-DB-Password", Is: "wrong"},
			{Key: "X-Session-Id", Is: "wrong"},
			{Key: "X-Debug-Info", Is: "expected"},
		},
		WantCookies: []ExpectCookie{
			{Name: "session", Is: "different"},
			{Name: "theme", Absent: true},
		},
	}
	failHdrs := http.Header{
		"Authorization": {"Bearer topsecretjwtvalue123456"},
		"X-Api-Key":     {"apikey-top-secret-val"},
		"Phpsessid":     {"phpsess-secret-val"},
		"X-User-Secret": {"user-secret-val"},
		"X-Db-Password": {"db-password-val"},
		"X-Session-Id":  {"sess-id-val"},
		"X-Debug-Info":  {"Bearer eyJhbGciOiJIUzI1NiJ9.eyJ1IjoxfQ.sig1234567890"},
	}
	failPage, _ := InspectResponse(ResponseInput{
		Method:      http.MethodGet,
		Body:        htmlDoc,
		ContentType: "text/html",
		Status:      200,
		Headers:     failHdrs,
		Cookies:     cookies,
		Step:        failStep,
		Base:        base,
	})
	if failPage.FailedChecks() != 9 {
		t.Fatalf("expected 9 failed checks, got %d: %+v", failPage.FailedChecks(), failPage.Checks)
	}
	for _, c := range failPage.Checks {
		for _, leak := range []string{
			"topsecretjwtvalue123456",
			"apikey-top-secret-val",
			"phpsess-secret-val",
			"user-secret-val",
			"db-password-val",
			"sess-id-val",
			"eyJhbGciOiJIUzI1NiJ9",
			"s_mid_supersecret_end",
			"dark",
		} {
			if strings.Contains(c.Detail, leak) {
				t.Errorf("SEC-10 violation on failed check %q detail %q (leaked %q)", c.Name, c.Detail, leak)
			}
		}
	}

	// Header NotContains must fail if ANY multi-value contains the forbidden substring.
	multiHdr := http.Header{"Cache-Control": {"private", "public"}}
	multiStep := &PlanItem{
		WantHeaders: []ExpectHeader{{Key: "Cache-Control", Contains: "private", NotContains: "public"}},
	}
	multiPage, _ := InspectResponse(ResponseInput{
		Method:      http.MethodGet,
		Body:        []byte("ok"),
		ContentType: "text/plain",
		Status:      200,
		Headers:     multiHdr,
		Step:        multiStep,
		Base:        base,
	})
	if multiPage.FailedChecks() != 1 {
		t.Errorf("expected multi-value NotContains failure, got %+v", multiPage.Checks)
	}
}

func TestInspectResponse_NotBlankRules(t *testing.T) {
	blankHTML := []byte(`<html><body></body></html>`)

	// 1. Blank 200 GET HTML fails not-blank.
	p1, _ := InspectPage(blankHTML, "text/html", 200, nil, testOrigin)
	if p1.FailedChecks() == 0 {
		t.Fatalf("expected blank 200 GET HTML to fail not-blank: %+v", p1.Checks)
	}

	// 2. SPA with script is NOT blank.
	spa, _ := InspectPage([]byte(`<html><body><div id="root"></div><script src="/app.js"></script></body></html>`), "text/html", 200, nil, testOrigin)
	if spa.FailedChecks() != 0 {
		t.Fatalf("expected SPA with script to pass: %+v", spa.Checks)
	}

	// 3. HEAD skips not-blank.
	headPage, _ := InspectResponse(ResponseInput{
		Method:      http.MethodHead,
		Body:        nil,
		ContentType: "text/html",
		Status:      200,
		Base:        testOrigin,
	})
	if headPage.FailedChecks() != 0 {
		t.Fatalf("expected HEAD to skip not-blank: %+v", headPage.Checks)
	}

	// 4. 204 and 304 skip not-blank.
	p204, _ := InspectResponse(ResponseInput{
		Method:      http.MethodDelete,
		Body:        nil,
		ContentType: "text/html",
		Status:      http.StatusNoContent,
		Base:        testOrigin,
	})
	if p204.FailedChecks() != 0 {
		t.Fatalf("expected 204 to skip not-blank: %+v", p204.Checks)
	}

	step304 := &PlanItem{WantStatus: StatusSpec{{Min: 304, Max: 304}}}
	p304, _ := InspectResponse(ResponseInput{
		Method:      http.MethodGet,
		Body:        nil,
		ContentType: "text/html",
		Status:      http.StatusNotModified,
		Step:        step304,
		Base:        testOrigin,
	})
	if p304.FailedChecks() != 0 {
		t.Fatalf("expected 304 to skip not-blank: %+v", p304.Checks)
	}

	// 5. Exclusively non-2xx want-status (e.g. 404 or 302) skips not-blank.
	step404 := &PlanItem{WantStatus: StatusSpec{{Min: 404, Max: 404}}}
	p404, _ := InspectResponse(ResponseInput{
		Method:      http.MethodGet,
		Body:        blankHTML,
		ContentType: "text/html",
		Status:      404,
		Step:        step404,
		Base:        testOrigin,
	})
	if p404.FailedChecks() != 0 {
		t.Fatalf("expected exclusively non-2xx step to skip not-blank: %+v", p404.Checks)
	}
}

func FuzzParsePlan(f *testing.F) {
	f.Add([]byte("plan:\n  - path: /\n"))
	f.Add([]byte("name: test\nweight: 2\nplan:\n  - name: login\n    path: /login\n    method: POST\n    want-status: \"200, 302\"\n    with-body:\n      - key: a\n        value: 1\n"))
	f.Add([]byte("plans:\n  - name: a\n    plan:\n      - path: /\n  - name: b\n    plan:\n      - path: /b\n"))
	f.Add([]byte("plan:\n  - path: /\n    want-headers:\n      - key: Server\n        is-one: [a, b]\n    want-cookies:\n      - name: sid\n        absent: true\n"))
	f.Add([]byte("---\nplan:\n  - path: /\n---\nplan:\n  - path: /2\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		plans, err := ParsePlanYAML(bytes.NewReader(data), "fuzz", testOrigin, 64)
		if err != nil {
			return
		}
		if len(plans) == 0 || len(plans) > MaxSuitePlans {
			t.Fatalf("unexpected plan count %d", len(plans))
		}
		for _, p := range plans {
			if p.Weight < 1 {
				t.Fatalf("invalid weight %d", p.Weight)
			}
			if len(p.Items) == 0 || len(p.Items) > 64 {
				t.Fatalf("unexpected item count %d", len(p.Items))
			}
			for _, item := range p.Items {
				if item.Chill != nil && *item.Chill < 0 {
					t.Fatalf("negative chill accepted: %v", *item.Chill)
				}
				if len(item.WithBody) > 0 && item.Body != "" {
					t.Fatalf("both with-body and body accepted")
				}
			}
		}
	})
}

func TestExtractErrorBody_HTML_JSON_XML_Text_AndScrub(t *testing.T) {
	hdr := http.Header{
		"X-Request-Id":  {"req-42"},
		"Retry-After":   {"5"},
		"Set-Cookie":    {"session=supersecretcookie"},
		"Authorization": {"Bearer secret-token-123"},
	}

	// 1. JSON Problem Details (RFC 7807)
	jsonBody := []byte(`{"type":"about:blank","title":"Gateway Fault","code":"E_UPSTREAM","error":"upstream timeout","detail":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJ1IjoxfQ.sig failed","request_id":"req-42"}`)
	exJSON := ExtractErrorBody(502, "application/problem+json", hdr, jsonBody)
	if exJSON.Format != "json" || exJSON.ErrorCode != "E_UPSTREAM" || exJSON.RequestID != "req-42" {
		t.Errorf("JSON extraction mismatch: %+v", exJSON)
	}
	if strings.Contains(exJSON.Summary+strings.Join(exJSON.Details, " ")+exJSON.BodyExcerpt, "eyJhbGciOiJIUzI1NiJ9") {
		t.Errorf("JSON extractor leaked bearer token: %+v", exJSON)
	}
	if _, leaked := exJSON.ResponseHeaders["Set-Cookie"]; leaked {
		t.Error("Set-Cookie must NOT appear in allowlisted ResponseHeaders")
	}
	if exJSON.ResponseHeaders["X-Request-Id"] != "req-42" {
		t.Errorf("expected allowlisted X-Request-Id header, got %v", exJSON.ResponseHeaders)
	}

	// 2. HTML error page with stack trace & script stripping
	htmlBody := []byte(`<!doctype html><html><head><title>500 Internal Error</title><script>var secret="x";</script></head>
		<body><h1>Database Pool Exhausted</h1><div class="alert">could not acquire pg connection</div>
		<pre>goroutine 1 [running]:
main.handleCheckout()
	/app/checkout.go:42</pre></body></html>`)
	exHTML := ExtractErrorBody(500, "text/html; charset=utf-8", hdr, htmlBody)
	if exHTML.Format != "html" || !strings.Contains(exHTML.Headline, "could not acquire pg connection") {
		t.Errorf("HTML extraction mismatch: %+v", exHTML)
	}
	if !strings.Contains(strings.Join(exHTML.Details, " "), "Database Pool Exhausted") {
		t.Errorf("expected h1 in HTML details, got %+v", exHTML.Details)
	}
	if !strings.Contains(exHTML.StackTrace, "goroutine 1") {
		t.Errorf("expected stack trace extraction, got %q", exHTML.StackTrace)
	}
	if strings.Contains(exHTML.BodyExcerpt, "var secret") {
		t.Errorf("script contents should be stripped from HTML excerpt: %q", exHTML.BodyExcerpt)
	}

	// 3. XML error body (S3/SOAP style)
	xmlBody := []byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>XML-99</RequestId></Error>`)
	exXML := ExtractErrorBody(403, "application/xml", nil, xmlBody)
	if exXML.Format != "xml" || exXML.ErrorCode != "AccessDenied" || exXML.RequestID != "XML-99" {
		t.Errorf("XML extraction mismatch: %+v", exXML)
	}

	// 4. Plain text
	exText := ExtractErrorBody(503, "text/plain", nil, []byte("503 Service Temporarily Unavailable\nTry again later"))
	if exText.Format != "text" || !strings.Contains(exText.Summary, "503 Service") {
		t.Errorf("text extraction mismatch: %+v", exText)
	}
}

func FuzzExtractErrorBody(f *testing.F) {
	f.Add(500, "text/html", []byte("<h1>Boom</h1><p>password=secret123</p>"))
	f.Add(502, "application/json", []byte(`{"error":"bad gateway","code":"E502"}`))
	f.Add(403, "application/xml", []byte(`<Error><Code>Denied</Code></Error>`))
	f.Add(503, "text/plain", []byte("overloaded"))
	f.Fuzz(func(t *testing.T, status int, ct string, body []byte) {
		ex := ExtractErrorBody(status, ct, nil, body)
		if len(ex.BodyExcerpt) > maxBodyExcerptBytes+16 {
			t.Fatalf("excerpt exceeded bound: %d", len(ex.BodyExcerpt))
		}
	})
}

func BenchmarkExtractErrorBody(b *testing.B) {
	hdr := http.Header{"X-Request-Id": {"req-bench-1"}, "Retry-After": {"2"}}
	htmlBody := []byte(`<!doctype html><html><head><title>500 Error</title></head><body><h1>Database Fault</h1><div class="alert">pool exhausted</div><pre>goroutine 1 [running]:\nmain.go:42</pre></body></html>`)
	jsonBody := []byte(`{"title":"Gateway Timeout","code":"E_TIMEOUT","error":"upstream timed out","request_id":"req-bench-1"}`)

	b.ResetTimer()
	for range b.N {
		_ = ExtractErrorBody(500, "text/html; charset=utf-8", hdr, htmlBody)
		_ = ExtractErrorBody(504, "application/problem+json", hdr, jsonBody)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
