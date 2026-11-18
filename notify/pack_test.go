package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the 'pack by whole item' fix: when the summary message exceeds the channel length limit, it must be **by whole item**
// truncated and truthfully report the number of items that didn't fit, so the caller only marks the ones actually delivered.
//
// The previous approach rendered the full piece then truncated, then marked the whole batch as delivered: the latter half of the message vanished into thin air,
// while the delivery history showed all successes — that's how the bug slipped through, and nowhere could it be detected.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 Chinese summary items, inevitably far exceeding WeCom's 4096 bytes.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body %d bytes exceeds limit %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should only fit a portion (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must truthfully state how many items this message contains and how many remain — otherwise readers will take the header
	// number as the total.
	if !strings.Contains(body, "remaining") || !strings.Contains(body, "continues in the next message") {
		t.Fatalf("header should state how many items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Should only contain the first kept items.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "bug"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "bug"+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("should keep all when length is unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "remaining") {
		t.Fatalf("truncation notice should not appear when not truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is too small to fit even one item, still send one message (backed by the final truncation).
	// Otherwise a single oversized bug would permanently stall the whole batch: every claim fits nothing, every time nothing is sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("should keep at least 1 item, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("single message should report 1 delivered, got %d", kept)
	}
	// An empty message has no deliverable entries.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("An empty message should report 0 entries, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits length by **character count**; using a byte measure would compress Chinese messages to a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("Body %d characters exceeds limit %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("Should fit only part, got %d", kept)
	}
	if !strings.Contains(text, "continued in next message") {
		t.Fatalf("Should indicate there is remaining content not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("Card should fit only part, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body; the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("Precondition not satisfied")
	}
	// Indirectly confirm via the renderer's return value: markdownBody(0) keeps everything when unlimited.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("Should use all when length is unlimited, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for 'untrusted content must not alter message structure'.
// Title and summary come from model output (the model reads the target's response); asset names come from the target's URL.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in the result (escaped form)
		wrong []string // must not appear in the result (unescaped form)
	}{
		{
			name: "newline + external link in title",
			item: Item{
				Severity: "high",
				Name:     "Login SQL injection\n[Urgent: verify account here](http://attacker.tld)",
			},
			// Newlines must be collapsed (otherwise new list items/blockquotes could be forged);
			// Square brackets and parentheses must be escaped (otherwise it is a clickable external link).
			must:  []string{`\[Urgent: verify account here\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "image beacon in title",
			item: Item{
				Severity: "high",
				Name:     "Vulnerability ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and quotes in asset name",
			item: Item{
				Severity: "high",
				Name:     "Plain Title",
				Assets:   []string{"a.com/*inject*>reference"},
			},
			must:  []string{`\*inject\*`, `\>`},
			wrong: []string{"*inject*"},
		},
		{
			name: "backticks and pipes in the summary",
			item: Item{
				Severity: "high",
				Name:     "Title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// WriteItem in single-item mode is the rendering path shared by the three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form %q appeared (can be used to inject structure or external links):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks the escape order: backslashes must be processed first,
// otherwise the backslashes added later get escaped again, and double backslashes appear in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escape order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks a specific regression:
// markdown escapes must not leak into Telegram's HTML output (escapes were once added
// in the shared title function, causing visible backslashes like `\(1\)` in Telegram messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("markdown backslash escapes appeared in the Telegram body:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
