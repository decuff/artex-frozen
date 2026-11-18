package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most important invariant in this package. WeCom limits length by **bytes**; Chinese is 3 bytes/char,
	// any implementation that hard-cuts by bytes will split a Chinese character in half, producing invalid UTF-8 that the platform rejects.
	// Use multiple Chinese-English mixed inputs with coprime lengths to hit every possible cut point.
	inputs := []string{
		"Chinese test content",
		"mixed mixed content content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4-byte emoji, wrong cuts are more obvious
		strings.Repeat("vulnerability", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result %d bytes exceeds limit", in, max, len(got))
			}
			// When not truncated, content must not be changed.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: content changed despite not exceeding limit -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean no limit")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean no limit")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, appending the ellipsis must not cause it to exceed the limit.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1: result %q length %d exceeds limit", got, len(got))
	}
	// Normal case should include the ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The difference from TruncateBytes must be preserved: Telegram limits by characters,
	// using byte semantics would cut a Chinese message down to only one third.
	s := "one two three four five six seven eight nine ten"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string should be noticeably shorter under byte semantics.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("byte semantics should not produce the same character count as character semantics")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("first line\n\nsecond line\twith tab   multiple spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("should collapse all whitespace, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("should not preserve consecutive spaces, got %q", got)
	}
	// After truncation it must still be readable and valid.
	got = OneLine("one two three four five six seven eight nine ten", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Truncating HTML directly can cut out fragments like `<a href="htt`, and the platform will reject the entire message.
	s := `<b>Title</b>body body body<a href="https://example.com/very/long/path">view details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result %d characters exceeds limit", max, n)
		}
		// The tail must not have an unclosed `<` (i.e., a `<` appears in the last segment without a `>`).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: tail tag was cut off -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a、b" {
		t.Fatalf("not exceeding limit should list all, got %q", got)
	}
	// When exceeding the limit, the total count must be noted, otherwise readers won't know how many assets weren't listed.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "and 5 more") {
		t.Fatalf("should note total count 5, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("empty level ordinal is 0, should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("empty threshold should pass")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("unknown status mapping, got %q", got)
	}
	// Unknown status is echoed as-is, no fabricated labels.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("unknown status should be echoed as-is, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers an omission pointed out by the audit: truncation must not only avoid
// half tags, but also avoid cut-off HTML entities.
//
// After `&amp;` is cut into `&amp`, a parser that only recognizes entities may reject the **entire** message—
// and overly long summary messages are already common, so the cost is too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain an entity fragment with '&' but no corresponding ';'.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: tail left an entity fragment %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity appeared", max)
		}
	}
}
