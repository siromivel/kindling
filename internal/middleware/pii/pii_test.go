package pii

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/spec"
)

// fakePresidio answers /analyze by finding fixed strings, reporting offsets
// in code points like the real analyzer.
type fakePresidio struct {
	known map[string]Result // text → entity type and score
	down  bool

	mu    sync.Mutex
	texts []string
}

func (f *fakePresidio) start(t *testing.T) *Analyzer {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down {
			http.Error(w, "boom: "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		var req struct {
			Text     string   `json:"text"`
			Entities []string `json:"entities"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.texts = append(f.texts, req.Text)
		f.mu.Unlock()
		results := []Result{}
		runes := []rune(req.Text)
		for s, res := range f.known {
			if !slices.Contains(req.Entities, res.EntityType) {
				continue
			}
			sub := []rune(s)
			for i := 0; i+len(sub) <= len(runes); i++ {
				if string(runes[i:i+len(sub)]) == s {
					results = append(results, Result{res.EntityType, i, i + len(sub), res.Score})
				}
			}
		}
		json.NewEncoder(w).Encode(results)
	}))
	t.Cleanup(srv.Close)
	return NewAnalyzer(srv.URL)
}

var standard = map[string]Rule{
	"US_SSN":        {spec.OperatorReplace, 0.3},
	"EMAIL_ADDRESS": {spec.OperatorReplace, 0.5},
	"PHONE_NUMBER":  {spec.OperatorMask, 0.5},
	"PERSON":        {spec.OperatorKeep, 0.5},
}

func TestRedactOperators(t *testing.T) {
	text := "Héllo, I'm Ann Lee: ann@example.com, 212-555-0123, SSN 536-22-8726"
	find := func(s string) (int, int) {
		i := strings.Index(text, s)
		return len([]rune(text[:i])), len([]rune(text[:i])) + len([]rune(s))
	}
	res := func(typ, s string, score float64) Result {
		start, end := find(s)
		return Result{typ, start, end, score}
	}
	out, findings := Redact(text, []Result{
		res("PERSON", "Ann Lee", 0.85),
		res("EMAIL_ADDRESS", "ann@example.com", 1),
		res("PHONE_NUMBER", "212-555-0123", 0.75),
		res("US_SSN", "536-22-8726", 0.35), // over its 0.3 threshold
	}, standard)

	want := "Héllo, I'm Ann Lee: <EMAIL_ADDRESS>, ***-***-0123, SSN <US_SSN>"
	if out != want {
		t.Errorf("Redact:\n got %q\nwant %q", out, want)
	}
	slices.SortFunc(findings, func(a, b Finding) int { return strings.Compare(a.Entity, b.Entity) })
	wantFindings := []Finding{{"EMAIL_ADDRESS", "replace"}, {"PERSON", "keep"}, {"PHONE_NUMBER", "mask"}, {"US_SSN", "replace"}}
	if !slices.Equal(findings, wantFindings) {
		t.Errorf("findings = %v, want %v", findings, wantFindings)
	}
}

func TestRedactThresholds(t *testing.T) {
	text := "ssn 536-22-8726 mail a@b.io"
	out, findings := Redact(text, []Result{
		{"US_SSN", 4, 15, 0.29},        // just under 0.3
		{"EMAIL_ADDRESS", 21, 27, 0.5}, // exactly the threshold counts
		{"LOCATION", 0, 3, 0.99},       // not in the policy
	}, standard)
	if out != "ssn 536-22-8726 mail <EMAIL_ADDRESS>" || len(findings) != 1 {
		t.Errorf("Redact = %q, %v", out, findings)
	}
}

func TestRedactOverlaps(t *testing.T) {
	// A keep entity inside a redacted one doesn't protect it, and partly
	// overlapping redactions are merged so no detected character leaks.
	text := "contact ann.lee@example.com or 4111 1111 1111 1111"
	out, findings := Redact(text, []Result{
		{"PERSON", 8, 15, 0.9},
		{"EMAIL_ADDRESS", 8, 27, 1},
		{"CREDIT_CARD", 31, 50, 1},
		{"PHONE_NUMBER", 36, 50, 0.4},
	}, map[string]Rule{
		"PERSON":        {spec.OperatorKeep, 0.5},
		"EMAIL_ADDRESS": {spec.OperatorReplace, 0.5},
		"CREDIT_CARD":   {spec.OperatorReplace, 0.3},
		"PHONE_NUMBER":  {spec.OperatorMask, 0.3},
	})
	if out != "contact <EMAIL_ADDRESS> or <CREDIT_CARD>" {
		t.Errorf("Redact = %q", out)
	}
	if !slices.Equal(findings, []Finding{{"EMAIL_ADDRESS", "replace"}, {"CREDIT_CARD", "replace"}}) {
		t.Errorf("findings = %v", findings)
	}
}

func TestMask(t *testing.T) {
	for in, want := range map[string]string{
		"212-555-0123":     "***-***-0123",
		"(212) 555-0123":   "(***) ***-0123",
		"4111111111111111": "************1111",
		"Bob":              "***", // too short to show anything
		"555-0123":         "***-0123",
	} {
		if got := string(mask([]rune(in))); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func request(t *testing.T, body string) *middleware.ChatRequest {
	t.Helper()
	req, err := middleware.ParseChatRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func contents(t *testing.T, req *middleware.ChatRequest) []string {
	t.Helper()
	var out []string
	for _, m := range req.Messages {
		texts, err := m.Texts()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join(texts, "|"))
	}
	return out
}

func TestStage(t *testing.T) {
	fake := &fakePresidio{known: map[string]Result{
		"ann@example.com": {EntityType: "EMAIL_ADDRESS", Score: 1},
		"212-555-0123":    {EntityType: "PHONE_NUMBER", Score: 0.75},
		"Ann Lee":         {EntityType: "PERSON", Score: 0.85},
	}}
	stage := New(fake.start(t), Policy{Name: "standard", Roles: []string{"user", "system"}, Rules: standard}, middleware.FailClosed)
	req := request(t, `{"model":"chat","messages":[
		{"role":"system","content":"Support for Ann Lee"},
		{"role":"user","content":[{"type":"text","text":"mail ann@example.com"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"or 212-555-0123"}]},
		{"role":"assistant","content":"I'll mail ann@example.com"}]}`)

	trace, root := middleware.NewTrace("test")
	run := (&middleware.Chain{Stages: []middleware.Stage{stage}}).Start(req, root, nil)
	if resp, err := run.Request(context.Background()); err != nil || resp != nil {
		t.Fatalf("Request = %v, %v", resp, err)
	}
	want := []string{
		"Support for Ann Lee", // PERSON is keep
		"mail <EMAIL_ADDRESS>|or ***-***-0123",
		"I'll mail ann@example.com", // assistant isn't in roles
	}
	if got := contents(t, req); !slices.Equal(got, want) {
		t.Errorf("messages = %q, want %q", got, want)
	}
	if len(fake.texts) != 3 {
		t.Errorf("analyzed %d texts, want 3 (system + two user text parts)", len(fake.texts))
	}

	var piiSpan *middleware.Span
	for _, s := range trace.Spans() {
		if s.Name == "pii.request" {
			piiSpan = s
		}
	}
	attrs := piiSpan.Attrs()
	for k, v := range map[string]any{
		"kindling.outcome":                    OutcomeRedacted,
		"kindling.pii.policy":                 "standard",
		"kindling.pii.redacted":               2,
		"kindling.pii.EMAIL_ADDRESS.count":    1,
		"kindling.pii.EMAIL_ADDRESS.operator": "replace",
		"kindling.pii.PHONE_NUMBER.operator":  "mask",
		"kindling.pii.PERSON.count":           1,
		"kindling.pii.PERSON.operator":        "keep",
	} {
		if attrs[k] != v {
			t.Errorf("span %s = %v, want %v", k, attrs[k], v)
		}
	}
	// The span never holds what was matched.
	for k, v := range attrs {
		s, _ := v.(string)
		for _, secret := range []string{"ann@", "555", "Ann Lee"} {
			if strings.Contains(k, secret) || strings.Contains(s, secret) {
				t.Errorf("span attribute %s=%v leaks %q", k, v, secret)
			}
		}
	}
}

func TestStageFailClosed(t *testing.T) {
	fake := &fakePresidio{down: true}
	stage := New(fake.start(t), Policy{Name: "p", Roles: []string{"user"}, Rules: standard}, middleware.FailClosed)
	body := `{"model":"chat","messages":[{"role":"user","content":"mail ann@example.com"}]}`
	req := request(t, body)
	run := (&middleware.Chain{Stages: []middleware.Stage{stage}}).Start(req, nil, nil)
	_, err := run.Request(context.Background())
	var se *middleware.StageError
	if !errors.As(err, &se) || se.Stage != "pii" {
		t.Fatalf("err = %v, want a pii StageError", err)
	}
	if strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q includes the analyzer's response body", err)
	}
	if got := contents(t, req); got[0] != "mail ann@example.com" {
		t.Errorf("request changed on failure: %q", got)
	}
}

func TestStageFailOpen(t *testing.T) {
	fake := &fakePresidio{down: true}
	stage := New(fake.start(t), Policy{Name: "p", Roles: []string{"user"}, Rules: standard}, middleware.FailOpen)
	req := request(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	run := (&middleware.Chain{Stages: []middleware.Stage{stage}}).Start(req, nil, nil)
	if _, err := run.Request(context.Background()); err != nil {
		t.Errorf("fail_open stage returned %v", err)
	}
}
