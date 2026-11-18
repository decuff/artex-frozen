package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations must be **stateless**: the same instance will be reused concurrently by multiple channel
// configs, and credentials are always passed in via the cfg parameter.
type Channel interface {
	// Kind returns the channel type identifier, which must match the registry key.
	Kind() string
	// Validate is called when saving config, validating required fields and format. The returned error is shown directly to
	// the configurer, so the message should say 'which field is missing' rather than a vague 'config invalid'.
	Validate(cfg map[string]any) error
	// Send delivers one message, returning the **number of entries actually delivered** and an error.
	//
	// Why return the count: every platform has a message length limit, and when the aggregated message can't fit the whole batch it gets truncated.
	// If the caller unconditionally marks the whole batch as delivered, the truncated entries just vanish—invisible in the message,
	// shown as successful in delivery history, and nowhere can you discover that a gap was never sent. After returning kept,
	// the caller only marks the first kept entries, leaving the rest for the next batch.
	//
	// A returned error means delivery failed, where *PermanentError means it should not be retried.
	// On failure kept is meaningless and the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the channel's officially recommended per-minute delivery limit, used as the default rate limit
	// when creating a new channel instance. Returning 0 means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the key names in the channel config that are credentials. When echoed by the API these keys' values are masked,
	// and on update a masked value means keep the original value in the store. Only the implementation itself knows which fields count as credentials
	// (for WeCom the entire Webhook URL is the credential, while for DingTalk it's only the secret within it),
	// so this knowledge must be provided by the channel, not guessed by the upper layer.
	SecretKeys() []string
	// DestinationKeys returns the key names in the channel config that determine 'where the message is sent'.
	//
	// Like SecretKeys it's security-related: destination addresses and credentials are two independent sets of fields,
	// and if 'change only the address, keep credentials as-is' were allowed, anyone who can edit channel config could send the real credentials in the store
	// to a server they control, making channel config masking completely meaningless.
	// See PrepareConfigUpdate for details.
	DestinationKeys() []string
}

// registry is the channel registry. Deliberately using an explicit literal instead of init() self-registration: this way 'which channels exist'
// can be seen all in one place, and adding a channel exposes omissions at compile time rather than relying on runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get returns the channel implementation by type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (for stable display in UI dropdowns).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried: credential errors, target rejection, invalid request body, etc.
// Retrying only makes sense for transient failures (network jitter, rate limiting, peer 5xx); repeatedly backing off and retrying permanent failures
// will neither succeed nor keep the real error from being buried in retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil,
// convenient for writing `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the err chain carries a permanent failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- Config reading helpers ----
//
// Channel config comes from a JSONB column in the database; after encoding/json deserialization it is map[string]any,
// numbers are always float64, arrays are []any. The helpers below unify this conversion layer and tolerate type deviations
// caused by users leaving fields blank in the UI (e.g. entering a port as a string).

// cfgString gets a string config item, trimming leading and trailing whitespace—easy to pick up when copy-pasting from web forms.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt gets an integer config item, compatible with both float64 (JSON default) and string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool gets a boolean config item, compatible with the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings gets a string array config item, automatically trimming whitespace and discarding empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accepts a single string, convenient for form submission when there is only one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap gets a string map config item (e.g. custom HTTP headers), trimming whitespace from keys and values, discarding empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
