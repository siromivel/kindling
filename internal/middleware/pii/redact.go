package pii

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/siromivel/kindling/internal/spec"
)

// Rule is how a policy treats one entity type.
type Rule struct {
	Operator  string // spec.OperatorReplace, OperatorMask or OperatorKeep
	Threshold float64
}

// Finding is one entity that was acted on. It never holds the matched text.
type Finding struct {
	Entity, Operator string
}

// Redact applies rules to the analyzer's results for text. Results below
// their entity's threshold, or for entities without a rule, are ignored.
//
// The results' offsets must index the same string, in code points, so the
// analyzer must have been sent exactly ValidUTF8(text).
//
// Overlapping (or touching) results are merged so that nothing detected
// escapes: each group of replace/mask results is redacted as a whole, with
// the most protective operator among them (replace over mask). keep results
// that overlap a redaction are dropped, since what they matched isn't
// forwarded.
func Redact(text string, results []Result, rules map[string]Rule) (string, []Finding) {
	text = ValidUTF8(text)
	runes := []rune(text)
	var redact, keep []Result
	for _, r := range results {
		rule, ok := rules[r.EntityType]
		r.Start, r.End = max(r.Start, 0), min(r.End, len(runes))
		if !ok || r.Score < rule.Threshold || r.Start >= r.End {
			continue
		}
		if rule.Operator == spec.OperatorKeep {
			keep = append(keep, r)
		} else {
			redact = append(redact, r)
		}
	}

	clusters := cluster(redact, rules)
	var findings []Finding
	for _, c := range clusters {
		findings = append(findings, Finding{c.EntityType, rules[c.EntityType].Operator})
	}
	for _, c := range cluster(keep, rules) {
		overlaps := slices.ContainsFunc(clusters, func(r Result) bool { return c.Start < r.End && r.Start < c.End })
		if !overlaps {
			findings = append(findings, Finding{c.EntityType, spec.OperatorKeep})
		}
	}
	if len(clusters) == 0 {
		return text, findings
	}

	// Rewrite from the end so earlier offsets stay valid.
	for _, c := range slices.Backward(clusters) {
		var with []rune
		if rules[c.EntityType].Operator == spec.OperatorMask {
			with = mask(runes[c.Start:c.End])
		} else {
			with = []rune("<" + c.EntityType + ">")
		}
		runes = slices.Concat(runes[:c.Start], with, runes[c.End:])
	}
	return string(runes), findings
}

// cluster merges overlapping or touching results into one per group,
// spanning the whole group and labeled with its strongest member: the most
// protective operator, then the highest score, then the longest match. So a
// mask can never leave part of something another detection replaces visible.
// The result is sorted by position.
func cluster(results []Result, rules map[string]Rule) []Result {
	slices.SortFunc(results, func(a, b Result) int { return cmp.Compare(a.Start, b.Start) })
	var out []Result
	for _, r := range results {
		if n := len(out); n > 0 && r.Start <= out[n-1].End {
			c := &out[n-1]
			if stronger(r, *c, rules) {
				c.EntityType, c.Score = r.EntityType, r.Score
			}
			c.End = max(c.End, r.End)
			continue
		}
		out = append(out, r)
	}
	return out
}

func stronger(a, b Result, rules map[string]Rule) bool {
	if pa, pb := protection(rules[a.EntityType].Operator), protection(rules[b.EntityType].Operator); pa != pb {
		return pa > pb
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.End-a.Start > b.End-b.Start
}

// protection ranks operators by how much of a value they hide.
func protection(op string) int {
	switch op {
	case spec.OperatorReplace:
		return 2
	case spec.OperatorMask:
		return 1
	}
	return 0
}

// ValidUTF8 returns s with each run of invalid bytes replaced by U+FFFD.
// Presidio's offsets are Python string indexes and Redact's are Go rune
// indexes; they agree only if both sides see the same valid string, so the
// stage normalizes each text once and uses the result for both.
func ValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, string(utf8.RuneError))
}

// maskVisible is how many trailing characters mask leaves readable, and
// maskMinLength the length a match needs before any are: shorter matches
// are masked entirely, so masking never reveals most of a short value.
const (
	maskVisible   = 4
	maskMinLength = 8
)

// mask hides the letters and digits of s except the last four characters,
// keeping separators, so 212-555-0123 becomes ***-***-0123.
func mask(s []rune) []rune {
	out := slices.Clone(s)
	visible := len(out) - maskVisible
	if len(out) < maskMinLength {
		visible = len(out)
	}
	for i := range visible {
		if unicode.IsLetter(out[i]) || unicode.IsDigit(out[i]) {
			out[i] = '*'
		}
	}
	return out
}
