// Package plan implements the YAML Plan & multi-persona Suite engine for
// Lemmings, along with HTTP response inspection, asset extraction, and
// structured error-body diagnostics.
package plan

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"gopkg.in/yaml.v3"
)

// Navigation modes for -navigation and suite limits.
const (
	NavigationLinks     = "links"
	NavigationRandom    = "random"
	DefaultMaxPlanItems = 999
	MaxSuitePlans       = 100
)

const (
	minPlanReadBytes     int64 = 1 << 20  // 1 MiB floor
	maxPlanReadBytes     int64 = 64 << 20 // 64 MiB ceiling
	planReadBytesPerItem int64 = 4096
)

var (
	simpleSelector = regexp.MustCompile(`^(?:[a-zA-Z][a-zA-Z0-9-]*|[#.][a-zA-Z_][a-zA-Z0-9_-]*)$`)

	allowedMethods = map[string]bool{
		http.MethodGet:     true,
		http.MethodPost:    true,
		http.MethodPut:     true,
		http.MethodPatch:   true,
		http.MethodDelete:  true,
		http.MethodHead:    true,
		http.MethodOptions: true,
	}
)

// NameValue is a canonical name/value pair in YAML that also accepts "key" as
// an unambiguous alias for "name" (rejecting entries where both are set).
// Scalar values such as numbers or booleans decode cleanly to strings.
type NameValue struct {
	Name  string `yaml:"name" json:"-"`
	Value string `yaml:"value" json:"-"`
}

// UnmarshalYAML decodes a NameValue mapping strictly, rejecting unknown keys,
// duplicate keys, and entries specifying both "name" and "key".
func (nv *NameValue) UnmarshalYAML(node *yaml.Node) error {
	k, v, err := decodePairNode(node, "name", "key")
	if err != nil {
		return err
	}
	nv.Name = k
	nv.Value = v
	return nil
}

// KeyValue is a canonical key/value pair in YAML that also accepts "name" as
// an unambiguous alias for "key" (rejecting entries where both are set).
// Scalar values such as numbers or booleans decode cleanly to strings.
type KeyValue struct {
	Key   string `yaml:"key" json:"-"`
	Value string `yaml:"value" json:"-"`
}

// UnmarshalYAML decodes a KeyValue mapping strictly, rejecting unknown keys,
// duplicate keys, and entries specifying both "key" and "name".
func (kv *KeyValue) UnmarshalYAML(node *yaml.Node) error {
	k, v, err := decodePairNode(node, "key", "name")
	if err != nil {
		return err
	}
	kv.Key = k
	kv.Value = v
	return nil
}

// PlanCookie is a cookie entry seeded via with-cookies (using NameValue semantics).
type PlanCookie struct {
	Name  string `yaml:"name" json:"-"`
	Value string `yaml:"value" json:"-"`
}

// UnmarshalYAML decodes a PlanCookie using NameValue semantics.
func (c *PlanCookie) UnmarshalYAML(node *yaml.Node) error {
	k, v, err := decodePairNode(node, "name", "key")
	if err != nil {
		return err
	}
	c.Name = k
	c.Value = v
	return nil
}

// PlanHeader is a per-step HTTP request header via with-headers (using KeyValue semantics).
type PlanHeader struct {
	Key   string `yaml:"key" json:"-"`
	Value string `yaml:"value" json:"-"`
}

// UnmarshalYAML decodes a PlanHeader using KeyValue semantics.
func (h *PlanHeader) UnmarshalYAML(node *yaml.Node) error {
	k, v, err := decodePairNode(node, "key", "name")
	if err != nil {
		return err
	}
	h.Key = k
	h.Value = v
	return nil
}

// PlanBody is a key/value request body field via with-body (using KeyValue semantics).
type PlanBody struct {
	Key   string `yaml:"key" json:"-"`
	Value string `yaml:"value" json:"-"`
}

// UnmarshalYAML decodes a PlanBody using KeyValue semantics.
func (b *PlanBody) UnmarshalYAML(node *yaml.Node) error {
	k, v, err := decodePairNode(node, "key", "name")
	if err != nil {
		return err
	}
	b.Key = k
	b.Value = v
	return nil
}

// StringList decodes either a YAML sequence of scalar values or a
// comma-separated scalar string.
type StringList []string

// UnmarshalYAML decodes a StringList from a YAML sequence or comma-separated scalar.
func (sl *StringList) UnmarshalYAML(node *yaml.Node) error {
	node = derefAlias(node)
	if node == nil {
		return errors.New("is-one must not be empty")
	}
	switch node.Kind {
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return fmt.Errorf("line %d: is-one list must not be empty", node.Line)
		}
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			s, err := scalarToString(item, "is-one")
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		*sl = out
		return nil
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return fmt.Errorf("line %d: is-one must not be empty", node.Line)
		}
		raw, err := scalarToString(node, "is-one")
		if err != nil {
			return err
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) == 0 {
			return fmt.Errorf("line %d: is-one must contain at least one value", node.Line)
		}
		*sl = out
		return nil
	default:
		return fmt.Errorf("line %d: is-one must be a string or list of strings", node.Line)
	}
}

// ExpectHeader asserts response header presence and/or value matchers.
// When no matcher is specified, only header presence is checked.
type ExpectHeader struct {
	Key         string     `yaml:"key" json:"key"`
	Contains    string     `yaml:"contains,omitempty" json:"contains,omitempty"`
	NotContains string     `yaml:"not-contains,omitempty" json:"not_contains,omitempty"`
	Is          string     `yaml:"is,omitempty" json:"is,omitempty"`
	BeginsWith  string     `yaml:"begins-with,omitempty" json:"begins_with,omitempty"`
	EndsWith    string     `yaml:"ends-with,omitempty" json:"ends_with,omitempty"`
	IsOne       StringList `yaml:"is-one,omitempty" json:"is_one,omitempty"`
}

// UnmarshalYAML decodes ExpectHeader strictly, accepting "name" as an alias
// for "key" while rejecting entries that specify both or contain unknown keys.
func (eh *ExpectHeader) UnmarshalYAML(node *yaml.Node) error {
	node = derefAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		line := nodeLine(node)
		return fmt.Errorf("line %d: want-headers entry must be a mapping", line)
	}
	var out ExpectHeader
	seen := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		kNode := derefAlias(node.Content[i])
		vNode := derefAlias(node.Content[i+1])
		k, err := scalarToString(kNode, "want-headers")
		if err != nil {
			return err
		}
		if seen[k] {
			return fmt.Errorf("line %d: duplicate field %q in want-headers", kNode.Line, k)
		}
		seen[k] = true
		switch k {
		case "key", "name":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.Key = val
		case "contains":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.Contains = val
		case "not-contains":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.NotContains = val
		case "is":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.Is = val
		case "begins-with":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.BeginsWith = val
		case "ends-with":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.EndsWith = val
		case "is-one":
			if err := out.IsOne.UnmarshalYAML(vNode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("line %d: unknown field %q in want-headers", kNode.Line, k)
		}
	}
	if seen["key"] && seen["name"] {
		return fmt.Errorf("line %d: want-headers entry cannot set both \"key\" and \"name\"", node.Line)
	}
	*eh = out
	return nil
}

// ExpectCookie asserts active cookie presence, absence, or value matchers.
// Value is an alias for exact Is (mutually exclusive). Absent cannot be
// combined with any value matcher.
type ExpectCookie struct {
	Name            string     `yaml:"name" json:"name"`
	Value           string     `yaml:"value,omitempty" json:"-"`
	Is              string     `yaml:"is,omitempty" json:"-"`
	Contains        string     `yaml:"contains,omitempty" json:"-"`
	NotContains     string     `yaml:"not-contains,omitempty" json:"-"`
	BeginsWith      string     `yaml:"begins-with,omitempty" json:"-"`
	EndsWith        string     `yaml:"ends-with,omitempty" json:"-"`
	IsOne           StringList `yaml:"is-one,omitempty" json:"-"`
	Absent          bool       `yaml:"absent,omitempty" json:"absent,omitempty"`
	hasValueMatcher bool
}

func (ec ExpectCookie) hasAnyMatcher() bool {
	return ec.hasValueMatcher ||
		ec.Value != "" ||
		ec.Is != "" ||
		ec.Contains != "" ||
		ec.NotContains != "" ||
		ec.BeginsWith != "" ||
		ec.EndsWith != "" ||
		len(ec.IsOne) > 0
}

func (ec ExpectCookie) exactValue() string {
	if ec.Is != "" {
		return ec.Is
	}
	return ec.Value
}

// UnmarshalYAML decodes ExpectCookie strictly, accepting "key" as an alias for
// "name" and "value" as an alias for "is", while rejecting ambiguous dual keys,
// unknown keys, and combining absent: true with value matchers.
func (ec *ExpectCookie) UnmarshalYAML(node *yaml.Node) error {
	node = derefAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		line := nodeLine(node)
		return fmt.Errorf("line %d: want-cookies entry must be a mapping", line)
	}
	var out ExpectCookie
	seen := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		kNode := derefAlias(node.Content[i])
		vNode := derefAlias(node.Content[i+1])
		k, err := scalarToString(kNode, "want-cookies")
		if err != nil {
			return err
		}
		if seen[k] {
			return fmt.Errorf("line %d: duplicate field %q in want-cookies", kNode.Line, k)
		}
		seen[k] = true
		switch k {
		case "name", "key":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.Name = val
		case "value":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.Value = val
			out.hasValueMatcher = true
		case "is":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.Is = val
			out.hasValueMatcher = true
		case "contains":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.Contains = val
			out.hasValueMatcher = true
		case "not-contains":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.NotContains = val
			out.hasValueMatcher = true
		case "begins-with":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.BeginsWith = val
			out.hasValueMatcher = true
		case "ends-with":
			val, err := scalarToString(vNode, k)
			if err != nil {
				return err
			}
			out.EndsWith = val
			out.hasValueMatcher = true
		case "is-one":
			if err := out.IsOne.UnmarshalYAML(vNode); err != nil {
				return err
			}
			out.hasValueMatcher = true
		case "absent":
			if vNode == nil || vNode.Kind != yaml.ScalarNode || vNode.Tag != "!!bool" {
				return fmt.Errorf("line %d: field \"absent\" in want-cookies must be a boolean", nodeLine(vNode))
			}
			if err := vNode.Decode(&out.Absent); err != nil {
				return fmt.Errorf("line %d: invalid \"absent\" boolean: %w", vNode.Line, err)
			}
		default:
			return fmt.Errorf("line %d: unknown field %q in want-cookies", kNode.Line, k)
		}
	}
	if seen["name"] && seen["key"] {
		return fmt.Errorf("line %d: want-cookies entry cannot set both \"name\" and \"key\"", node.Line)
	}
	if seen["value"] && seen["is"] {
		return fmt.Errorf("line %d: want-cookies entry cannot set both \"value\" and \"is\"", node.Line)
	}
	if out.Absent && out.hasValueMatcher {
		return fmt.Errorf("line %d: want-cookies entry cannot combine \"absent: true\" with value matchers", node.Line)
	}
	*ec = out
	return nil
}

// ExpectBody asserts response body substring inclusion and/or exclusion.
type ExpectBody struct {
	Contains    string `yaml:"contains,omitempty" json:"contains,omitempty"`
	NotContains string `yaml:"not-contains,omitempty" json:"not_contains,omitempty"`
}

// StatusRange is an inclusive HTTP status code range [Min, Max].
type StatusRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// String formats the StatusRange as "200" or "200-299".
func (r StatusRange) String() string {
	if r.Min == r.Max {
		return strconv.Itoa(r.Min)
	}
	return fmt.Sprintf("%d-%d", r.Min, r.Max)
}

// StatusSpec is a list of acceptable HTTP status ranges.
// An empty StatusSpec matches any 2xx status (200..299).
type StatusSpec []StatusRange

// IsEmpty reports whether no explicit status ranges were configured.
func (s StatusSpec) IsEmpty() bool {
	return len(s) == 0
}

// Matches reports whether HTTP status code matches this spec (empty = 200..299).
func (s StatusSpec) Matches(code int) bool {
	if len(s) == 0 {
		return code >= 200 && code <= 299
	}
	for _, r := range s {
		if code >= r.Min && code <= r.Max {
			return true
		}
	}
	return false
}

// Allows2xx reports whether this spec permits at least one 2xx status code.
func (s StatusSpec) Allows2xx() bool {
	if len(s) == 0 {
		return true
	}
	for _, r := range s {
		if r.Min <= 299 && r.Max >= 200 {
			return true
		}
	}
	return false
}

// Exclusively3xx reports whether this spec is non-empty and every allowed code
// lies within 300..399.
func (s StatusSpec) Exclusively3xx() bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if r.Min < 300 || r.Max > 399 {
			return false
		}
	}
	return true
}

// String renders the StatusSpec as a comma-separated list (or "2xx" when empty).
func (s StatusSpec) String() string {
	if len(s) == 0 {
		return "2xx"
	}
	parts := make([]string, len(s))
	for i, r := range s {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

// ParseStatusSpec parses a status specification string such as "200",
// "200,201", "2xx", "200-299", or comma-separated combinations thereof.
func ParseStatusSpec(raw string) (StatusSpec, error) {
	parts := strings.Split(raw, ",")
	out := make(StatusSpec, 0, len(parts))
	for _, p := range parts {
		r, err := parseStatusToken(p)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("status spec must not be empty")
	}
	return out, nil
}

// UnmarshalYAML decodes StatusSpec from a scalar int, scalar string, or YAML
// sequence of ints/strings.
func (s *StatusSpec) UnmarshalYAML(node *yaml.Node) error {
	node = derefAlias(node)
	if node == nil {
		return errors.New("want-status must not be empty")
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			*s = nil
			return nil
		}
		if node.Tag != "!!int" && node.Tag != "!!str" {
			return fmt.Errorf("line %d: want-status must be an integer, string, or list, got %s", node.Line, node.Tag)
		}
		parsed, err := ParseStatusSpec(node.Value)
		if err != nil {
			return fmt.Errorf("line %d: invalid want-status %q: %w", node.Line, node.Value, err)
		}
		*s = parsed
		return nil
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return fmt.Errorf("line %d: want-status list must not be empty", node.Line)
		}
		var out StatusSpec
		for _, elem := range node.Content {
			elem = derefAlias(elem)
			if elem == nil || elem.Kind != yaml.ScalarNode || (elem.Tag != "!!int" && elem.Tag != "!!str") {
				return fmt.Errorf("line %d: want-status list elements must be integers or strings", nodeLine(elem))
			}
			parsed, err := ParseStatusSpec(elem.Value)
			if err != nil {
				return fmt.Errorf("line %d: invalid want-status element %q: %w", elem.Line, elem.Value, err)
			}
			out = append(out, parsed...)
		}
		*s = out
		return nil
	default:
		return fmt.Errorf("line %d: want-status must be an integer, string, or list", node.Line)
	}
}

func parseStatusToken(tok string) (StatusRange, error) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return StatusRange{}, errors.New("empty status token")
	}
	if len(tok) == 3 && tok[0] >= '1' && tok[0] <= '5' && (tok[1] == 'x' || tok[1] == 'X') && (tok[2] == 'x' || tok[2] == 'X') {
		base := int(tok[0]-'0') * 100
		return StatusRange{Min: base, Max: base + 99}, nil
	}
	if strings.Contains(tok, "-") {
		parts := strings.Split(tok, "-")
		if len(parts) != 2 {
			return StatusRange{}, fmt.Errorf("invalid status range %q", tok)
		}
		lo, err := parseStatusCode(parts[0])
		if err != nil {
			return StatusRange{}, fmt.Errorf("invalid status range %q: %w", tok, err)
		}
		hi, err := parseStatusCode(parts[1])
		if err != nil {
			return StatusRange{}, fmt.Errorf("invalid status range %q: %w", tok, err)
		}
		if lo > hi {
			return StatusRange{}, fmt.Errorf("invalid status range %q: min %d > max %d", tok, lo, hi)
		}
		return StatusRange{Min: lo, Max: hi}, nil
	}
	code, err := parseStatusCode(tok)
	if err != nil {
		return StatusRange{}, err
	}
	return StatusRange{Min: code, Max: code}, nil
}

func parseStatusCode(s string) (int, error) {
	s = strings.TrimSpace(s)
	code, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid status code %q", s)
	}
	if code < 100 || code > 599 {
		return 0, fmt.Errorf("status code %d out of range 100..599", code)
	}
	return code, nil
}

// PlanItem is one ordered HTTP step in a Plan.
type PlanItem struct {
	Name            string         `yaml:"name" json:"name"`
	Path            string         `yaml:"path" json:"path"`
	Method          string         `yaml:"method,omitempty" json:"method,omitempty"`
	Chill           *time.Duration `yaml:"chill,omitempty" json:"chill,omitempty"`
	FollowRedirects *bool          `yaml:"follow-redirects,omitempty" json:"follow_redirects,omitempty"`
	StopOnFail      *bool          `yaml:"stop-on-fail,omitempty" json:"stop_on_fail,omitempty"`
	WithCookies     []PlanCookie   `yaml:"with-cookies,omitempty" json:"-"`
	WithHeaders     []PlanHeader   `yaml:"with-headers,omitempty" json:"-"`
	WithBody        []PlanBody     `yaml:"with-body,omitempty" json:"-"`
	Body            string         `yaml:"body,omitempty" json:"-"`
	WantStatus      StatusSpec     `yaml:"want-status,omitempty" json:"want_status,omitempty"`
	WantHeaders     []ExpectHeader `yaml:"want-headers,omitempty" json:"want_headers,omitempty"`
	WantCookies     []ExpectCookie `yaml:"want-cookies,omitempty" json:"want_cookies,omitempty"`
	WantBody        []ExpectBody   `yaml:"want-body,omitempty" json:"want_body,omitempty"`
	WantContentType string         `yaml:"want-content-type,omitempty" json:"want_content_type,omitempty"`
	WantTitle       string         `yaml:"want-title,omitempty" json:"want_title,omitempty"`
	WantSelectors   []string       `yaml:"want-selectors,omitempty" json:"want_selectors,omitempty"`
}

// EffectiveMethod returns the canonical uppercase HTTP method, defaulting to GET.
func (item PlanItem) EffectiveMethod() string {
	m := strings.ToUpper(strings.TrimSpace(item.Method))
	if m == "" {
		return http.MethodGet
	}
	return m
}

// ShouldFollowRedirects reports whether HTTP redirects should be followed for
// this step. When FollowRedirects is unset, redirects are followed unless
// WantStatus expects exclusively 3xx codes.
func (item PlanItem) ShouldFollowRedirects() bool {
	if item.FollowRedirects != nil {
		return *item.FollowRedirects
	}
	return !item.WantStatus.Exclusively3xx()
}

// ShouldStopOnFail reports whether a failing step should abort the remaining
// plan for the lemming, falling back to planDefault when unset on the step.
func (item PlanItem) ShouldStopOnFail(planDefault bool) bool {
	if item.StopOnFail != nil {
		return *item.StopOnFail
	}
	return planDefault
}

// HasRequestBody reports whether the step carries a request payload via
// WithBody or Body.
func (item PlanItem) HasRequestBody() bool {
	return len(item.WithBody) > 0 || item.Body != ""
}

// Plan is one ordered sequence of HTTP steps representing a user persona or scenario.
type Plan struct {
	Name       string     `yaml:"name,omitempty" json:"name"`
	Weight     int        `yaml:"weight,omitempty" json:"weight"`
	StopOnFail bool       `yaml:"stop-on-fail,omitempty" json:"stop_on_fail,omitempty"`
	Loop       bool       `yaml:"loop,omitempty" json:"loop,omitempty"`
	Items      []PlanItem `yaml:"plan" json:"plan"`
}

// HasNonGET reports whether any step in the plan uses an HTTP method other than GET.
func (p *Plan) HasNonGET() bool {
	if p == nil {
		return false
	}
	for _, item := range p.Items {
		if item.EffectiveMethod() != http.MethodGet {
			return true
		}
	}
	return false
}

// Suite holds one or more validated Plans assigned to non-rogue lemmings in
// proportion to each Plan's Weight.
type Suite struct {
	Plans       []*Plan `json:"plans"`
	TotalWeight int     `json:"total_weight"`
	TotalSteps  int     `json:"total_steps"`
}

// NewSuite validates suite-wide invariants (1..MaxSuitePlans, unique names,
// positive weights) and constructs a Suite from plans.
func NewSuite(plans []*Plan) (*Suite, error) {
	if len(plans) == 0 {
		return nil, errors.New("plan suite must contain at least 1 plan")
	}
	if len(plans) > MaxSuitePlans {
		return nil, fmt.Errorf("plan suite cannot exceed %d plans (got %d)", MaxSuitePlans, len(plans))
	}
	seen := make(map[string]bool, len(plans))
	totalWeight := 0
	totalSteps := 0
	for i, p := range plans {
		if p == nil {
			return nil, fmt.Errorf("plan suite entry %d is nil", i)
		}
		if strings.TrimSpace(p.Name) == "" {
			return nil, fmt.Errorf("plan suite entry %d has empty name", i)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("duplicate plan name %q in suite", p.Name)
		}
		seen[p.Name] = true
		if p.Weight < 0 {
			return nil, fmt.Errorf("plan %q: weight must be >= 0, got %d", p.Name, p.Weight)
		}
		if p.Weight == 0 {
			p.Weight = 1
		}
		if totalWeight > math.MaxInt-p.Weight {
			return nil, fmt.Errorf("plan suite total weight overflows int at plan %q", p.Name)
		}
		totalWeight += p.Weight
		totalSteps += len(p.Items)
	}
	return &Suite{
		Plans:       plans,
		TotalWeight: totalWeight,
		TotalSteps:  totalSteps,
	}, nil
}

// Pick deterministically selects a Plan for the given 0-based plan cohort index
// in proportion to each Plan's Weight.
func (s *Suite) Pick(planCohortIndex int64) *Plan {
	if s == nil || len(s.Plans) == 0 || s.TotalWeight <= 0 {
		return nil
	}
	w := int64(s.TotalWeight)
	slot := planCohortIndex % w
	if slot < 0 {
		slot += w
	}
	var acc int64
	for _, p := range s.Plans {
		acc += int64(p.Weight)
		if slot < acc {
			return p
		}
	}
	return s.Plans[len(s.Plans)-1]
}

// HasNonGET reports whether any plan in the suite contains a non-GET step.
func (s *Suite) HasNonGET() bool {
	if s == nil {
		return false
	}
	for _, p := range s.Plans {
		if p.HasNonGET() {
			return true
		}
	}
	return false
}

// PrimaryName returns the first plan's name, or "" if the suite is empty.
func (s *Suite) PrimaryName() string {
	if s == nil || len(s.Plans) == 0 {
		return ""
	}
	return s.Plans[0].Name
}

// Names returns a slice of all plan names in suite order.
func (s *Suite) Names() []string {
	if s == nil || len(s.Plans) == 0 {
		return nil
	}
	out := make([]string, len(s.Plans))
	for i, p := range s.Plans {
		out[i] = p.Name
	}
	return out
}

// LoadSuite loads one or more YAML plan files or directories specified as a
// comma-separated list in spec, validating every step against hit's origin and
// enforcing 1..maxItems steps per plan and 1..MaxSuitePlans total plans.
func LoadSuite(spec, hit string, maxItems int) (*Suite, error) {
	if maxItems <= 0 {
		maxItems = DefaultMaxPlanItems
	}
	if strings.TrimSpace(spec) == "" {
		return nil, errors.New("plan spec must not be empty")
	}

	rawEntries := strings.Split(spec, ",")
	var files []string
	for _, rawEntry := range rawEntries {
		entry := strings.TrimSpace(rawEntry)
		if entry == "" {
			return nil, fmt.Errorf("plan spec %q contains an empty entry", spec)
		}
		info, err := os.Stat(entry)
		if err != nil {
			return nil, fmt.Errorf("plan %q: %w", entry, err)
		}
		if info.IsDir() {
			dirEntries, err := os.ReadDir(entry)
			if err != nil {
				return nil, fmt.Errorf("read plan directory %q: %w", entry, err)
			}
			var dirFiles []string
			for _, de := range dirEntries {
				if de.IsDir() {
					continue
				}
				ext := strings.ToLower(filepath.Ext(de.Name()))
				if ext != ".yaml" && ext != ".yml" {
					continue
				}
				full := filepath.Join(entry, de.Name())
				if de.Type()&os.ModeSymlink != 0 {
					st, err := os.Stat(full)
					if err != nil {
						return nil, fmt.Errorf("plan %q: %w", full, err)
					}
					if st.IsDir() {
						continue
					}
				}
				dirFiles = append(dirFiles, full)
			}
			if len(dirFiles) == 0 {
				return nil, fmt.Errorf("plan directory %q contains no .yaml or .yml files", entry)
			}
			files = append(files, dirFiles...)
		} else {
			files = append(files, entry)
		}
	}

	var allPlans []*Plan
	for _, file := range files {
		plans, err := loadPlanFile(file, hit, maxItems)
		if err != nil {
			return nil, err
		}
		allPlans = append(allPlans, plans...)
		if len(allPlans) > MaxSuitePlans {
			return nil, fmt.Errorf("plan suite cannot exceed %d plans (got at least %d)", MaxSuitePlans, len(allPlans))
		}
	}
	return NewSuite(allPlans)
}

func loadPlanFile(file, hit string, maxItems int) ([]*Plan, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("open plan %q: %w", file, err)
	}
	defer f.Close() // read-only handle

	base := filepath.Base(file)
	defaultName := strings.TrimSuffix(base, filepath.Ext(base))
	plans, err := ParsePlanYAML(f, defaultName, hit, maxItems)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return plans, nil
}

// ParsePlanYAML decodes and validates either a single-plan YAML document
// (top-level `plan:`) or a multi-persona bundle YAML document (top-level
// `plans:`) using strict KnownFields(true) decoding and single-document
// enforcement.
func ParsePlanYAML(r io.Reader, defaultName, hit string, maxItems int) ([]*Plan, error) {
	if maxItems <= 0 {
		maxItems = DefaultMaxPlanItems
	}
	limit := planReadLimit(maxItems)
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read plan %q: %w", defaultName, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("plan %q exceeds maximum size of %d bytes", defaultName, limit)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var raw rawPlanFile
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("plan %q: YAML document is empty", defaultName)
		}
		return nil, fmt.Errorf("plan %q: %w", defaultName, err)
	}

	var trailing yaml.Node
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("plan %q: invalid trailing YAML content: %w", defaultName, err)
		}
		return nil, fmt.Errorf("plan %q: file must contain a single YAML document", defaultName)
	}

	if raw.Plan != nil && raw.Plans != nil {
		return nil, fmt.Errorf("plan %q: cannot specify both top-level \"plan\" and \"plans\"", defaultName)
	}
	if raw.Plan == nil && raw.Plans == nil {
		return nil, fmt.Errorf("plan %q: must specify top-level \"plan\" or \"plans\"", defaultName)
	}

	baseName := strings.TrimSpace(defaultName)
	if baseName == "" {
		baseName = "plan"
	}

	var plans []*Plan
	if raw.Plan != nil {
		rp := rawPlan{
			Name:       raw.Name,
			Weight:     raw.Weight,
			StopOnFail: raw.StopOnFail,
			Loop:       raw.Loop,
			Items:      raw.Plan,
		}
		p, err := buildPlan(rp, baseName, hit, maxItems)
		if err != nil {
			return nil, err
		}
		plans = []*Plan{p}
	} else {
		if raw.Name != "" || raw.Weight != 0 || raw.StopOnFail || raw.Loop {
			return nil, fmt.Errorf("plan %q: top-level name, weight, stop-on-fail, and loop cannot be set alongside \"plans\"", defaultName)
		}
		if len(*raw.Plans) == 0 {
			return nil, fmt.Errorf("plan %q: \"plans\" must contain at least 1 plan", defaultName)
		}
		if len(*raw.Plans) > MaxSuitePlans {
			return nil, fmt.Errorf("plan %q: cannot exceed %d plans (got %d)", defaultName, MaxSuitePlans, len(*raw.Plans))
		}
		plans = make([]*Plan, 0, len(*raw.Plans))
		seenNames := make(map[string]bool, len(*raw.Plans))
		for i, rp := range *raw.Plans {
			fallback := fmt.Sprintf("%s-%d", baseName, i+1)
			p, err := buildPlan(rp, fallback, hit, maxItems)
			if err != nil {
				return nil, err
			}
			if seenNames[p.Name] {
				return nil, fmt.Errorf("duplicate plan name %q", p.Name)
			}
			seenNames[p.Name] = true
			plans = append(plans, p)
		}
	}

	return plans, nil
}

func planReadLimit(maxItems int) int64 {
	if maxItems <= 0 {
		maxItems = DefaultMaxPlanItems
	}
	if int64(maxItems) > maxPlanReadBytes/planReadBytesPerItem {
		return maxPlanReadBytes
	}
	limit := int64(maxItems) * planReadBytesPerItem
	if limit < minPlanReadBytes {
		return minPlanReadBytes
	}
	if limit > maxPlanReadBytes {
		return maxPlanReadBytes
	}
	return limit
}

// --- Internal raw YAML decoding types (preserve KnownFields(true) everywhere) ---

type yamlDuration time.Duration

func (d *yamlDuration) UnmarshalYAML(node *yaml.Node) error {
	node = derefAlias(node)
	if node == nil || node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: chill must be a duration string (e.g. \"30s\", \"500ms\", \"0s\")", nodeLine(node))
	}
	if node.Tag == "!!null" {
		return nil
	}
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return fmt.Errorf("line %d: chill duration must not be empty", node.Line)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("line %d: invalid chill duration %q: %w", node.Line, node.Value, err)
	}
	if parsed < 0 {
		return fmt.Errorf("line %d: chill duration %q must not be negative", node.Line, node.Value)
	}
	*d = yamlDuration(parsed)
	return nil
}

type rawPlanFile struct {
	Name       string         `yaml:"name"`
	Weight     int            `yaml:"weight"`
	StopOnFail bool           `yaml:"stop-on-fail"`
	Loop       bool           `yaml:"loop"`
	Plan       *[]rawPlanItem `yaml:"plan"`
	Plans      *[]rawPlan     `yaml:"plans"`
}

type rawPlan struct {
	Name       string         `yaml:"name"`
	Weight     int            `yaml:"weight"`
	StopOnFail bool           `yaml:"stop-on-fail"`
	Loop       bool           `yaml:"loop"`
	Items      *[]rawPlanItem `yaml:"plan"`
}

type rawPlanItem struct {
	Name            string         `yaml:"name"`
	Path            string         `yaml:"path"`
	Method          string         `yaml:"method"`
	Chill           *yamlDuration  `yaml:"chill"`
	FollowRedirects *bool          `yaml:"follow-redirects"`
	StopOnFail      *bool          `yaml:"stop-on-fail"`
	WithCookies     []PlanCookie   `yaml:"with-cookies"`
	WithHeaders     []PlanHeader   `yaml:"with-headers"`
	WithBody        *[]PlanBody    `yaml:"with-body"`
	Body            *string        `yaml:"body"`
	WantStatus      StatusSpec     `yaml:"want-status"`
	WantHeaders     []ExpectHeader `yaml:"want-headers"`
	WantCookies     []ExpectCookie `yaml:"want-cookies"`
	WantBody        []ExpectBody   `yaml:"want-body"`
	WantContentType string         `yaml:"want-content-type"`
	WantTitle       string         `yaml:"want-title"`
	WantSelectors   []string       `yaml:"want-selectors"`
}

func buildPlan(rp rawPlan, fallbackName, hit string, maxItems int) (*Plan, error) {
	name := strings.TrimSpace(rp.Name)
	if name == "" {
		name = fallbackName
	}
	if rp.Weight < 0 {
		return nil, fmt.Errorf("plan %q: weight must be >= 0, got %d", name, rp.Weight)
	}
	weight := rp.Weight
	if weight == 0 {
		weight = 1
	}

	var rawItems []rawPlanItem
	if rp.Items != nil {
		rawItems = *rp.Items
	}
	if len(rawItems) == 0 {
		return nil, fmt.Errorf("plan %q must contain at least 1 plan item", name)
	}
	if len(rawItems) > maxItems {
		return nil, fmt.Errorf("cannot have more than %d plan items in %q — have %d (override with -max-plan-items)", maxItems, name, len(rawItems))
	}

	items := make([]PlanItem, len(rawItems))
	for i, ri := range rawItems {
		item, err := convertAndValidateItem(name, i, ri, hit)
		if err != nil {
			return nil, err
		}
		items[i] = item
	}

	return &Plan{
		Name:       name,
		Weight:     weight,
		StopOnFail: rp.StopOnFail,
		Loop:       rp.Loop,
		Items:      items,
	}, nil
}

func convertAndValidateItem(planName string, idx int, ri rawPlanItem, hit string) (PlanItem, error) {
	stepName := strings.TrimSpace(ri.Name)
	if stepName == "" {
		stepName = fmt.Sprintf("step-%d", idx+1)
	}

	if strings.TrimSpace(ri.Path) == "" || discovery.ResolveURL(hit, ri.Path) == "" {
		return PlanItem{}, fmt.Errorf("plan %q step %q: path %q must stay on %s", planName, stepName, ri.Path, discovery.SafeURL(hit))
	}

	method := strings.ToUpper(strings.TrimSpace(ri.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !allowedMethods[method] {
		return PlanItem{}, fmt.Errorf("plan %q step %q: unsupported HTTP method %q", planName, stepName, ri.Method)
	}

	var chill *time.Duration
	if ri.Chill != nil {
		d := time.Duration(*ri.Chill)
		if d < 0 {
			return PlanItem{}, fmt.Errorf("plan %q step %q: chill must not be negative", planName, stepName)
		}
		chill = &d
	}

	if ri.WithBody != nil && ri.Body != nil {
		return PlanItem{}, fmt.Errorf("plan %q step %q: with-body and body are mutually exclusive", planName, stepName)
	}

	// Validate and env-expand with-cookies.
	var withCookies []PlanCookie
	if len(ri.WithCookies) > 0 {
		withCookies = make([]PlanCookie, len(ri.WithCookies))
		for j, c := range ri.WithCookies {
			if strings.TrimSpace(c.Name) == "" {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-cookies[%d] name must not be empty", planName, stepName, j)
			}
			if hasControlChars(c.Name) {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-cookies[%d] name must not contain CR, LF, or NUL", planName, stepName, j)
			}
			val, err := expandEnvStrict(c.Value)
			if err != nil {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-cookies[%d] %q: %w", planName, stepName, j, c.Name, err)
			}
			if hasControlChars(val) {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-cookies[%d] %q value must not contain CR, LF, or NUL", planName, stepName, j, c.Name)
			}
			withCookies[j] = PlanCookie{Name: c.Name, Value: val}
		}
	}

	// Validate and env-expand with-headers.
	var withHeaders []PlanHeader
	if len(ri.WithHeaders) > 0 {
		withHeaders = make([]PlanHeader, len(ri.WithHeaders))
		for j, h := range ri.WithHeaders {
			if strings.TrimSpace(h.Key) == "" {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-headers[%d] key must not be empty", planName, stepName, j)
			}
			if hasControlChars(h.Key) {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-headers[%d] key must not contain CR, LF, or NUL", planName, stepName, j)
			}
			if strings.EqualFold(strings.TrimSpace(h.Key), "Host") {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-headers[%d] cannot override Host header", planName, stepName, j)
			}
			val, err := expandEnvStrict(h.Value)
			if err != nil {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-headers[%d] %q: %w", planName, stepName, j, h.Key, err)
			}
			if hasControlChars(val) {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-headers[%d] %q value must not contain CR, LF, or NUL", planName, stepName, j, h.Key)
			}
			withHeaders[j] = PlanHeader{Key: h.Key, Value: val}
		}
	}

	// Validate and env-expand with-body or body.
	var withBody []PlanBody
	if ri.WithBody != nil && len(*ri.WithBody) > 0 {
		withBody = make([]PlanBody, len(*ri.WithBody))
		for j, b := range *ri.WithBody {
			if strings.TrimSpace(b.Key) == "" {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-body[%d] key must not be empty", planName, stepName, j)
			}
			val, err := expandEnvStrict(b.Value)
			if err != nil {
				return PlanItem{}, fmt.Errorf("plan %q step %q: with-body[%d] %q: %w", planName, stepName, j, b.Key, err)
			}
			withBody[j] = PlanBody{Key: b.Key, Value: val}
		}
	}

	var rawBody string
	if ri.Body != nil {
		expanded, err := expandEnvStrict(*ri.Body)
		if err != nil {
			return PlanItem{}, fmt.Errorf("plan %q step %q: body: %w", planName, stepName, err)
		}
		rawBody = expanded
	}

	// Validate want-status ranges.
	for _, r := range ri.WantStatus {
		if r.Min < 100 || r.Max > 599 || r.Min > r.Max {
			return PlanItem{}, fmt.Errorf("plan %q step %q: invalid want-status range %d-%d", planName, stepName, r.Min, r.Max)
		}
	}

	// Validate want-headers.
	for j, h := range ri.WantHeaders {
		if strings.TrimSpace(h.Key) == "" {
			return PlanItem{}, fmt.Errorf("plan %q step %q: want-headers[%d] key must not be empty", planName, stepName, j)
		}
		if hasControlChars(h.Key) {
			return PlanItem{}, fmt.Errorf("plan %q step %q: want-headers[%d] key must not contain CR, LF, or NUL", planName, stepName, j)
		}
	}

	// Validate want-cookies.
	for j, c := range ri.WantCookies {
		if strings.TrimSpace(c.Name) == "" {
			return PlanItem{}, fmt.Errorf("plan %q step %q: want-cookies[%d] name must not be empty", planName, stepName, j)
		}
		if hasControlChars(c.Name) {
			return PlanItem{}, fmt.Errorf("plan %q step %q: want-cookies[%d] name must not contain CR, LF, or NUL", planName, stepName, j)
		}
		if c.Value != "" && c.Is != "" {
			return PlanItem{}, fmt.Errorf("plan %q step %q: want-cookies[%d] %q cannot set both \"value\" and \"is\"", planName, stepName, j, c.Name)
		}
		if c.Absent && c.hasAnyMatcher() {
			return PlanItem{}, fmt.Errorf("plan %q step %q: want-cookies[%d] %q cannot combine \"absent: true\" with value matchers", planName, stepName, j, c.Name)
		}
	}

	// Validate want-body.
	for j, b := range ri.WantBody {
		if b.Contains == "" && b.NotContains == "" {
			return PlanItem{}, fmt.Errorf("plan %q step %q: want-body[%d] must specify contains or not-contains", planName, stepName, j)
		}
	}

	// Validate want-selectors.
	for _, sel := range ri.WantSelectors {
		if !simpleSelector.MatchString(sel) {
			return PlanItem{}, fmt.Errorf("plan %q step %q: selector %q must be a tag, #id or .class", planName, stepName, sel)
		}
	}

	return PlanItem{
		Name:            stepName,
		Path:            ri.Path,
		Method:          method,
		Chill:           chill,
		FollowRedirects: ri.FollowRedirects,
		StopOnFail:      ri.StopOnFail,
		WithCookies:     withCookies,
		WithHeaders:     withHeaders,
		WithBody:        withBody,
		Body:            rawBody,
		WantStatus:      ri.WantStatus,
		WantHeaders:     ri.WantHeaders,
		WantCookies:     ri.WantCookies,
		WantBody:        ri.WantBody,
		WantContentType: ri.WantContentType,
		WantTitle:       ri.WantTitle,
		WantSelectors:   ri.WantSelectors,
	}, nil
}

// --- Shared YAML & string helpers ---

func derefAlias(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode && depth < 16; depth++ {
		node = node.Alias
	}
	return node
}

func nodeLine(node *yaml.Node) int {
	if node == nil {
		return 0
	}
	return node.Line
}

func scalarToString(node *yaml.Node, field string) (string, error) {
	node = derefAlias(node)
	if node == nil || node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("line %d: field %q must be a scalar value", nodeLine(node), field)
	}
	if node.Tag == "!!null" {
		return "", nil
	}
	return node.Value, nil
}

func decodePairNode(node *yaml.Node, canonicalKey, aliasKey string) (string, string, error) {
	node = derefAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return "", "", fmt.Errorf("line %d: entry must be a mapping with %q and \"value\"", nodeLine(node), canonicalKey)
	}
	var keyVal, val string
	seen := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		kNode := derefAlias(node.Content[i])
		vNode := derefAlias(node.Content[i+1])
		k, err := scalarToString(kNode, canonicalKey)
		if err != nil {
			return "", "", err
		}
		if seen[k] {
			return "", "", fmt.Errorf("line %d: duplicate field %q", kNode.Line, k)
		}
		seen[k] = true
		switch k {
		case canonicalKey, aliasKey:
			s, err := scalarToString(vNode, k)
			if err != nil {
				return "", "", err
			}
			keyVal = s
		case "value":
			s, err := scalarToString(vNode, "value")
			if err != nil {
				return "", "", err
			}
			val = s
		default:
			return "", "", fmt.Errorf("line %d: unknown field %q (allowed: %q, %q, \"value\")", kNode.Line, k, canonicalKey, aliasKey)
		}
	}
	if seen[canonicalKey] && seen[aliasKey] {
		return "", "", fmt.Errorf("line %d: cannot set both %q and %q on the same entry", node.Line, canonicalKey, aliasKey)
	}
	return keyVal, val, nil
}

func hasControlChars(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

// expandEnvStrict expands ${ENV_VAR} references matching
// \$\{([A-Za-z_][A-Za-z0-9_]*)\} and unescapes $$ to a literal $.
// It fails closed naming the missing variable (never printing values) if any
// referenced variable is unset.
func expandEnvStrict(s string) (string, error) {
	if !strings.ContainsRune(s, '$') {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	n := len(s)
	for i := 0; i < n; {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < n && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+2 < n && s[i+1] == '{' {
			if closeRel := strings.IndexByte(s[i+2:], '}'); closeRel > 0 {
				name := s[i+2 : i+2+closeRel]
				if isEnvVarName(name) {
					val, ok := os.LookupEnv(name)
					if !ok {
						return "", fmt.Errorf("environment variable %s is not set", name)
					}
					b.WriteString(val)
					i += 2 + closeRel + 1
					continue
				}
			}
		}
		b.WriteByte('$')
		i++
	}
	return b.String(), nil
}

func isEnvVarName(s string) bool {
	if len(s) == 0 {
		return false
	}
	c0 := s[0]
	if !((c0 >= 'A' && c0 <= 'Z') || (c0 >= 'a' && c0 <= 'z') || c0 == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}
