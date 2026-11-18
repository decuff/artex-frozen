package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard limit (in bytes, not characters) on the markdown content of a WeCom group robot.
// This is the tightest limit among all six channels and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom group robot.
//
// Platform characteristics:
//   - Authenticates solely via the key in the URL, no signature support—so the webhook address itself is the entire credential.
//   - markdown content is capped at 4096 **bytes**, and the whole message is rejected if too long (not truncated). Chinese is 3 bytes/char,
//     meaning only a little over a thousand characters of body text fit, so client-side truncation is required.
//   - Rate limited to 20 messages/minute, also handled by client-side rate limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential: the Webhook (the key in the URL), and it does not support signing—
// the entire address is the whole credential, with no other fields needing masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only one field, the Webhook, which is both the destination and the credential, so there is no such thing as "a credential left over after changing the address."
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Invalid Webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// The summary batch can be very long (50 items × one line each + prefix), and 4096 bytes is easily exceeded.
	// Truncation is done here rather than relying on a platform error: rejection means the whole batch is lost, whereas truncation at least delivers the first several items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 means the API call limit was exceeded—the platform's rate-limit window rolls, so retrying after backoff is effective,
		// so it is explicitly classified as retryable. Reaching here means the client's rate_per_min is configured too aggressively;
		// retrying is only a fallback, and the real fix is to lower the rate limit for this channel.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limit %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means the webhook key is invalid—a permanent failure, retrying will not self-heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
