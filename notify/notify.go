// Package notify implements the IM / email push channel adapter layer for vulnerability findings.
//
// Layering: this package is a **leaf package** and depends only on the standard library. It knows nothing about the database or the server. Channel config
// is passed in as map[string]any (corresponding to the notification_channels.config JSONB column),
// and the content to push is passed in as Message. The benefit of this separation is that signature computation, UTF-8 truncation, and filter matching—
// the places that are truly error-prone—can be unit tested without PostgreSQL, and the host only needs to do orchestration on the server side.
//
// Concurrency convention: implementations of Channel must be **stateless**. The same Channel instance will be reused concurrently by multiple channel configs
// (even multiple bot instances of the same channel), and all credentials are always passed in via the cfg parameter.
// Things like webhook URLs must not be cached into fields of the implementation itself.
package notify

// Channel type identifiers. The values are also the valid set for notification_channels.kind, validated by the server side
// against a whitelist (same as findings.status; no DB CHECK, to make adding channels later easier).
const (
	KindDingTalk = "dingtalk" // DingTalk custom robot
	KindFeishu   = "feishu"   // Feishu (including Lark) custom robot
	KindWeCom    = "wecom"    // WeCom group robot
	KindWebhook  = "webhook"  // Generic Webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for a kind that is empty in config.
const InitKind = KindDingTalk

// severityRank maps vulnerability severity to a comparable ordinal. Unknown severities return 0, so any
// min_severity setting will keep unknown severities out—when in doubt, do not push, to avoid false-positive spam.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the ordinal of the severity; unknown severities return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the Chinese severity name with emoji, used for message titles and card colors.
// Unknown severities are echoed as-is, not invented.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low Risk"
	default:
		return severity
	}
}

// StatusLabel translates the disposition status into Chinese for status change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In Progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
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

// AtLeast determines whether severity meets the min threshold. An empty min means no threshold, so it always passes.
// Note that the rank of an unknown severity is 0, so it will be rejected by any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
