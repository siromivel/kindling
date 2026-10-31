package pii

import (
	"context"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/spec"
)

// combining is U+0301, so "e"+combining renders as one é but is two code
// points in both Go and Python.
var combining = string(rune(0x301))

func TestRedactUnicodeOffsets(t *testing.T) {
	// Offsets are code points: one each for an emoji outside the BMP (two
	// UTF-16 units), each part of a ZWJ sequence or flag, and a combining mark.
	// Each appears before, inside and after the match.
	for _, u := range []string{"😀😀", "👩‍💻", "🇺🇸", "e" + combining, "你好"} {
		email := "ann" + u + "@example.com"
		text := u + " mail " + email + " " + u + " ok"
		start := utf8.RuneCountInString(u + " mail ")
		out, _ := Redact(text, []Result{{"EMAIL_ADDRESS", start, start + utf8.RuneCountInString(email), 1}}, standard)
		if want := u + " mail <EMAIL_ADDRESS> " + u + " ok"; out != want {
			t.Errorf("Redact(%q) = %q, want %q", text, out, want)
		}
	}
}

func TestRedactInvalidUTF8(t *testing.T) {
	// Offsets index ValidUTF8(text), where the run of bad bytes is one U+FFFD.
	text := "\xff\xfe mail ann@example.com"
	normalized := ValidUTF8(text)
	if want := string(utf8.RuneError) + " mail ann@example.com"; normalized != want {
		t.Fatalf("ValidUTF8 = %q, want %q", normalized, want)
	}
	start := strings.Index(normalized, "ann@")
	start = utf8.RuneCountInString(normalized[:start])
	out, _ := Redact(text, []Result{{"EMAIL_ADDRESS", start, start + 15, 1}}, standard)
	if want := string(utf8.RuneError) + " mail <EMAIL_ADDRESS>"; out != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
}

func TestStageUnusualText(t *testing.T) {
	fake := &fakePresidio{known: map[string]Result{"ann@example.com": {EntityType: "EMAIL_ADDRESS", Score: 1}}}
	stage := New(fake.start(t), Policy{Name: "p", Roles: []string{"user"}, Rules: standard}, middleware.FailClosed)
	// Raw invalid bytes and a lone surrogate escape arrive in the request
	// body. JSON decoding replaces each invalid byte with its own U+FFFD,
	// unlike ValidUTF8, which replaces each run with one; that's why the
	// analyzer and Redact must be given the very same string.
	prefix := "😀👩‍💻 e" + combining + " \xff\xfe "
	body := `{"messages":[{"role":"user","content":"` + prefix + `\ud83d mail ann@example.com"}]}`
	req := request(t, body)
	run := (&middleware.Chain{Stages: []middleware.Stage{stage}}).Start(req, nil, nil)
	if _, err := run.Request(context.Background()); err != nil {
		t.Fatal(err)
	}
	fffd := string(utf8.RuneError)
	want := "😀👩‍💻 e" + combining + " " + fffd + fffd + " " + fffd + " mail <EMAIL_ADDRESS>"
	if got := contents(t, req)[0]; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if fake.texts[0] != strings.TrimSuffix(want, "<EMAIL_ADDRESS>")+"ann@example.com" {
		t.Errorf("analyzer got %q", fake.texts[0])
	}
}

func TestRedactMaskNeverRevealsReplaced(t *testing.T) {
	// The phone number (mask) out-scores the card number it overlaps
	// (replace), but the merged span is still replaced: masking it would show
	// the card's last digits.
	text := "card 4111 1111 1111 1111"
	out, findings := Redact(text, []Result{
		{"CREDIT_CARD", 5, 24, 0.6},
		{"PHONE_NUMBER", 10, 24, 0.9},
	}, map[string]Rule{"CREDIT_CARD": {spec.OperatorReplace, 0.3}, "PHONE_NUMBER": {spec.OperatorMask, 0.3}})
	if out != "card <CREDIT_CARD>" || !slices.Equal(findings, []Finding{{"CREDIT_CARD", "replace"}}) {
		t.Errorf("Redact = %q, %v", out, findings)
	}
}

var fuzzRules = map[string]Rule{
	"R1": {spec.OperatorReplace, 0.5},
	"R2": {spec.OperatorReplace, 0.2},
	"M1": {spec.OperatorMask, 0.5},
	"M2": {spec.OperatorMask, 0.2},
	"K":  {spec.OperatorKeep, 0.5},
	// "X" has no rule, so it's never acted on.
}

var fuzzEntities = []string{"R1", "R2", "M1", "M2", "K", "X"}

// FuzzRedactNoLeak checks the property redaction exists for: no character
// inside a replace or mask match survives, except the last 4 characters of a
// masked group (and separators, which masking keeps). Text is built from
// unique letters, so any letter in the output can be traced to its position.
//
//	go test ./internal/middleware/pii -fuzz FuzzRedactNoLeak
func FuzzRedactNoLeak(f *testing.F) {
	f.Add([]byte("abcdefghijklmnopqrstuvwxyz"), []byte{2, 10, 2, 255, 6, 12, 0, 255})
	f.Add([]byte("abcdefghijklmnop"), []byte{0, 9, 2, 200, 8, 8, 3, 100, 4, 3, 4, 255})
	f.Add([]byte("aaaa-bbbb-cccc-dddd"), []byte{0, 19, 3, 255, 5, 9, 0, 50})
	f.Add([]byte("x"), []byte{255, 255, 1, 255})
	// Numeric runes that aren't decimal digits: superscripts, circled numbers, fullwidth digits
	f.Add([]byte("SSN ¹²³-④⑤-６７８９ end"), []byte{4, 16, 1, 255, 0, 3, 2, 120})
	// Combining marks (NFD "é" = e + U+0301) and a precomposed é side by side
	f.Add([]byte("Jose\u0301 Jos\u00e9 Rene\u0301e"), []byte{0, 5, 0, 255, 6, 10, 1, 200, 11, 18, 2, 90})
	// Emoji: ZWJ family sequence, skin-tone modifier, flag (regional indicators)
	f.Add([]byte("call 👨‍👩‍👧 at 555-0123 👍🏽 🇺🇸 ok"), []byte{0, 12, 1, 255, 13, 21, 0, 180, 5, 9, 3, 60})
	// Zero-width space and joiner inside an email, plus a BOM
	f.Add([]byte("\ufeffjohn\u200b@exam\u200dple.com"), []byte{1, 20, 1, 255, 0, 6, 0, 100})
	// Bidi controls: right-to-left override and an isolate around a number
	f.Add([]byte("acct \u202e1234-5678\u202c and \u20665551234\u2069"), []byte{5, 16, 1, 255, 21, 30, 0, 255})
	// Mixed scripts: Arabic, Hebrew, CJK, Cyrillic
	f.Add([]byte("محمد 电话 13800138000 Иван שלום"), []byte{0, 4, 0, 255, 8, 19, 1, 200, 20, 24, 2, 150})
	// Invalid UTF-8: lone continuation byte, truncated sequence, encoded surrogate, overlong
	f.Add([]byte("ab\x80cd\xe2\x82ef\xed\xa0\x80gh\xc0\xafij"), []byte{0, 6, 1, 255, 3, 12, 0, 255})
	// Control characters: NUL, CRLF, tab, DEL
	f.Add([]byte("a\x00b\r\nc\td\x7f 555-123-4567"), []byte{0, 4, 2, 255, 9, 21, 1, 255})

	f.Fuzz(func(t *testing.T, layout, spans []byte) {
		if len(layout) > 300 || len(spans) > 4*20 {
			return
		}
		var runes []rune
		for i, b := range layout {
			if b%4 == 0 {
				runes = append(runes, rune("-@. "[b/4%4]))
			} else {
				runes = append(runes, rune(0x4E00+i)) // a unique CJK letter
			}
		}
		n := len(runes)
		var results []Result
		for i := 0; i+3 < len(spans); i += 4 {
			// Starts and ends can fall outside the text, like a buggy analyzer's.
			start := int(spans[i])%(n+3) - 1
			results = append(results, Result{
				EntityType: fuzzEntities[int(spans[i+2])%len(fuzzEntities)],
				Start:      start,
				End:        start + int(spans[i+1])%14 - 1,
				Score:      float64(spans[i+3]) / 255,
			})
		}

		out, _ := Redact(string(runes), slices.Clone(results), fuzzRules)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 output %q", out)
		}

		// Model the groups independently: a group is a run of positions
		// covered by replace/mask matches that pass their thresholds.
		covered := make([]bool, n)
		replaced := make([]bool, n) // covered by a replace match
		for _, r := range results {
			rule, ok := fuzzRules[r.EntityType]
			if !ok || r.Score < rule.Threshold || rule.Operator == spec.OperatorKeep {
				continue
			}
			for p := max(r.Start, 0); p < min(r.End, n); p++ {
				covered[p] = true
				replaced[p] = replaced[p] || rule.Operator == spec.OperatorReplace
			}
		}
		visible := make([]bool, n) // letters that must survive
		for p := 0; p < n; {
			if !covered[p] {
				visible[p] = true
				p++
				continue
			}
			end, anyReplace := p, false
			for end < n && covered[end] {
				anyReplace = anyReplace || replaced[end]
				end++
			}
			if !anyReplace && end-p >= maskMinLength {
				for q := end - maskVisible; q < end; q++ {
					visible[q] = true
				}
			}
			p = end
		}

		var survivors []int
		for _, r := range out {
			if p := int(r - 0x4E00); r >= 0x4E00 && p < n && runes[p] == r {
				survivors = append(survivors, p)
			}
		}
		var want []int
		for p := range n {
			if visible[p] && runes[p] >= 0x4E00 {
				want = append(want, p)
			}
		}
		if !slices.Equal(survivors, want) {
			t.Fatalf("text %q with %v:\n got %q\nletters at %v survived, want exactly %v", string(runes), results, out, survivors, want)
		}
	})
}

// FuzzRedactArbitrary checks that any text, valid UTF-8 or not, and any
// offsets, never panic or produce invalid UTF-8.
func FuzzRedactArbitrary(f *testing.F) {
	f.Add("\xff\xfe mail ann@example.com", 7, 22, 0)
	f.Add("😀 x", -5, 99, 2)
	f.Fuzz(func(t *testing.T, text string, start, end, entity int) {
		e := fuzzEntities[(entity%len(fuzzEntities)+len(fuzzEntities))%len(fuzzEntities)]
		out, _ := Redact(text, []Result{{e, start, end, 1}}, fuzzRules)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 output %q", out)
		}
	})
}
