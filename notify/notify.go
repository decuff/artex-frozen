// Package notify implements the IM / email push channel adapter layer for vulnerability findings.
//
// Layering: this package is a **leaf package** and depends only on the standard library. It knows nothing about the database or the server. Channel config
// is passed in as map[string]any (corresponding to the notification_channels.config JSONB column),
// and the content to push is passed in as Message. The benefit of this split is that signature computation, UTF-8 truncation, and filter matching—the
// places that are genuinely error-prone—can be unit-tested without PostgreSQL, and the host only needs to orchestrate on the server side.
//
// Concurrency convention: Channel implementations must be **stateless**. The same Channel instance is reused concurrently by multiple channel configs
// (even multiple bot instances of the same channel), all credentials are passed in via the cfg parameter,
// and caching things like webhook URLs into the implementation's own fields is not allowed.
package notify

// Channel type identifiers. The values are also the valid set for notification_channels.kind, verified by a whitelist on the server side
// (same as findings.status; no DB CHECK, to make adding channels easier later).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (including Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // Generic Webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for an empty kind in config.
const InitKind = KindDingTalk

// severityRank maps vulnerability severity to a comparable ordinal. Unknown severities return 0, so any
// min_severity setting will keep unknown severities out—when in doubt, do not push, to avoid false-positive spam.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the severity ordinal; unknown severities return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the severity name and emoji, used for message titles and card coloring.
// Unknown levels are echoed as-is; don't make them up.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel returns the display name of a disposition status for notifications.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In Progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Handled"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False Positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk Accepted"
	default:
		return status
	}
}

// AtLeast checks whether severity meets the min threshold. An empty min means no threshold, so everything passes.
// Note that unknown severity has rank 0, so any non-empty min will reject it (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
