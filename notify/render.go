package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, ensuring the result is valid UTF-8 and does not split characters.
//
// Why we must cut at character boundaries: WeCom group bot markdown has a hard limit of 4096 **bytes** (not
// characters), and one Chinese character is 3 bytes. Slicing directly by bytes would cut a Chinese character in half, producing invalid
// UTF-8—the platform would either reject the whole message or show replacement blocks. The approach here is to start from the budget position
// and back up to the nearest rune start byte (utf8.RuneStart checks continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. Append an ellipsis after truncation unless max is too small to fit the ellipsis.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: skip the ellipsis and truncate plainly, to avoid the result exceeding max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine compresses multiline text into a single line: collapse all whitespace, then truncate by character count.
// Used for the title line of IM messages—summaries often contain line breaks, and inserting them directly into a table/title breaks the layout.
// max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an ellipsis when exceeded.
// max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's unit: WeCom limits by bytes, Telegram limits by character count.
// Using the wrong unit won't error; it just makes messages much shorter than expected (1 Chinese character = 3 bytes,
// so cutting by bytes at 4096 leaves about 1365 characters). Both functions must be kept and chosen per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count, ensuring no half tags are produced.
//
// Truncating HTML directly by characters can cut out incomplete tags like `<a href="htt`; the platform parser will either
// error out and reject the whole message, or swallow the following body text as an attribute value. The approach here is: first truncate by characters,
// then check whether the tail has an unclosed `<`, and if so, back up to before it.
//
// No tag balancing (filling in </b> etc.): Telegram's HTML parser automatically closes unclosed tags,
// while implementing balancing ourselves would require handling quotes in attributes, comments, and self-closing tags—complexity out of proportion to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (no `>` after the last `<`), back off to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a truncated HTML entity (e.g. `&amp;` cut to `&amp`), back off as well.
	// In a parser that only recognizes entities, an entity fragment may cause the **entire message** to be rejected—a summary message
	// exceeding the length limit is already common, and it is not worth losing the whole notification over it.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items fit **completely** within the budget, so a summary message can pack whole items.
//
// Why pack whole items instead of rendering the full text and truncating: truncation makes the latter half of the items vanish into thin air,
// while their delivery records are still marked as delivered—neither the message nor the delivery history shows a trace,
// and the gap is silently lost. After packing whole items, items that don't fit stay in the store for the next batch,
// and the kept value returned to the caller is the number of items actually delivered in this message.
//
// Params: maxSize<=0 means no limit; reserve is the amount set aside for the message header/footer;
// size does the measuring (each platform differs: WeCom/DingTalk use bytes, Telegram uses character count—
// using the wrong measure won't error, it just compresses Chinese messages far below the limit);
// render renders the idx-th item into its actual text—length varies by content, so it cannot be estimated.
//
// Return at least 1 (as long as items remain). A single extremely long item must still be sent, with the caller's final
// truncation as a fallback; otherwise one oversized finding would permanently block the whole batch.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are the two measures for packItemCount, named to avoid bare
// func(s string) int closures at call sites, which would make the unit hard to tell at a glance.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list into a single display line; when there are more than limit, omit the rest and note the total.
// One finding may be anchored to dozens of assets, and listing them all would blow up the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " (" + itoa(len(assets)) + " total)"
}

// itoa is a short alias for strconv.Itoa, used only for building display text, avoiding importing strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
