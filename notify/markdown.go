package notify

import (
	"fmt"
	"strings"
)

// This file is the message rendering shared by the 'Markdown-family' channels (DingTalk, WeCom).
// Feishu uses card JSON, Telegram uses HTML, email uses HTML, each rendered in its own adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A single vulnerability may anchor dozens of assets,
// listing them all would blow up the message and add no informational value—nobody looks at domains past the 4th in an IM.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. An IM message is a 'prompt to go view details',
// not the report itself; the full content is in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message header (summary line + severity distribution + possible truncation notice)
// and the footer (platform link). When packing the whole message, this part is deducted from the budget to ensure the head and tail are not cut off—
// once the head and tail are truncated, the reader cannot even tell 'which batch this is, and how many more are not shown'.
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why it is mandatory: vulnerability titles, summaries, types, and asset display names all come from **untrusted sources**—
// titles and summaries come from model output (the model reads the response of the target under test), and asset urls are the full
// URLs obtained from scanning (including target-controllable query strings). Without escaping, a vulnerability with the title
//
//	Login portal SQL injection\n[Urgent: click here to verify account](http://attacker.tld)
//
// would render in the security engineer's DingTalk/Feishu as a **clickable external link**; and
// `![](http://attacker.tld/beacon)` would be fetched by the client at render time, which amounts to announcing
// 'this vulnerability has been viewed' and leaking the reader's IP. Even for non-malicious content, injected bold or
// blockquote can push the severe vulnerability below out past the fold line.
//
// The escape set covers title/link/emphasis/list/blockquote/strikethrough—the categories that change structure or produce clickable
// elements. `\` must be handled first, otherwise the backslashes added later would be escaped again.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText compresses untrusted text into a single line and escapes it, for use in markdown body text.
// Single-lining is the other half beyond escaping: a newline by itself can forge a new list item or blockquote,
// and escape characters cannot stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the title bar/card title of IM platforms); the content is the **unescaped original text**.
//
// Escaping is deliberately not done here: this title is shared by renderers in four contexts—markdown body, Telegram's
// HTML, plain_text of Feishu cards, as well as the JSON of generic Webhooks and email subjects. Each context has
// different escaping rules (markdown escaping stuffed into HTML leaves visible backslashes, stuffed into JSON pollutes
// the data), so escaping must be handled by each output end, see writeItem / feishuItemLines /
// telegramEscape. We once added markdown escaping in a shared function, and as a result Telegram messages
// showed visible backslashes like `\(1\)`.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability Summary · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability Notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and **the number of items actually written**.
//
// The return value kept is the number of items actually delivered in this delivery; the caller uses it to mark only the first kept items as
// delivered—items blocked by the channel length limit must be left for the next batch, rather than being
// marked successful along with the rest. This is exactly the source of 'silent loss': the message was truncated, but the delivery record shows all delivered,
// and nowhere can you see that the latter half was never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even if it is too long (the final truncation catches it): partial information about one vulnerability
		// is better than sending none at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the beginning of the summary message: time window, count, and severity distribution.
// With these, recipients of the summary can judge whether this batch needs immediate attention without clicking into the platform.
//
// items are the items **actually fit in**, total is the total number this batch should have. When the two differ, it must be stated explicitly
// 'how many more are in the next message'—otherwise readers will think the number in the message header is all of them,
// and those items never sent simply do not exist in the interface.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new vulnerabilities in the last %d minutes**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**%d new vulnerabilities**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(this message shows the first %d, the remaining %d will continue in the next message)", len(items), extra)
	}
	// Give the distribution by severity so readers can see at a glance whether there are critical items. Only count those **actually included in this message**
	// Item, ensuring 'Severity 3' matches the items countable below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single vulnerability item.
//
// prefix is used for the index in the summary list; when single=true, render the full version (including summary and backlink),
// in the summary list only render a one-line summary—otherwise 50 summary entries would become a long document.
//
// All externally-sourced content (title/type/asset/summary) goes through markdownText:
// single-line + escaped. The backlink is assembled from the admin-configured public_base_url, is not untrusted content,
// and must be a clickable link, so it is output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Summary mode: single-line presentation, with asset and summary compressed and appended after.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Asset**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
