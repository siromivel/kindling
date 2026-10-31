package pii

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
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

	// Clusters are sorted and disjoint, so one forward pass rewrites them all.
	var out strings.Builder
	out.Grow(len(text))
	prev := 0
	for _, c := range clusters {
		out.WriteString(string(runes[prev:c.Start]))
		if rules[c.EntityType].Operator == spec.OperatorMask {
			out.WriteString(string(mask(runes[c.Start:c.End])))
		} else {
			out.WriteByte('<')
			out.WriteString(c.EntityType)
			out.WriteByte('>')
		}
		prev = c.End
	}
	out.WriteString(string(runes[prev:]))
	return out.String(), findings
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

// protection ranks operators by how much of a value they hide. Redact
// replaces anything that isn't mask or keep, so it ranks with replace.
func protection(op string) int {
	switch op {
	case spec.OperatorKeep:
		return 0
	case spec.OperatorMask:
		return 1
	}
	return 2
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

func isSeparator(r rune) bool {
	switch r {
	case ' ', '-', '.', '@', '(', ')', '/', '+', '_':
		return true
	}
	return false
}

// isSeparatorCluster reports whether a grapheme cluster is a lone separator.
// A separator carrying combining marks ("-" + U+0301) is not, so it is masked.
func isSeparatorCluster(c string) bool {
	r := []rune(c)
	return len(r) == 1 && isSeparator(r[0])
}

// mask hides every non-separator grapheme of s except the last four. It
// counts grapheme clusters (what a reader sees as one character), not runes,
// so a value's mask has the same shape whether it arrived precomposed (NFC)
// or decomposed (NFD), and a multi-rune emoji is masked as one unit. Values
// shorter than eight graphemes keep only their separators. Each masked
// grapheme becomes one '*', so the output can have fewer runes than s.
func mask(s []rune) []rune {
	var clusters []string
	rest, state := string(s), -1
	for len(rest) > 0 {
		var c string
		c, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		clusters = append(clusters, c)
	}

	maskUpTo := len(clusters) - maskVisible
	if len(clusters) < maskMinLength {
		maskUpTo = len(clusters)
	}

	out := make([]rune, 0, len(s))
	for i, c := range clusters {
		if i < maskUpTo && !isSeparatorCluster(c) {
			out = append(out, '*')
			continue
		}
		out = append(out, []rune(c)...)
	}
	return out
}
