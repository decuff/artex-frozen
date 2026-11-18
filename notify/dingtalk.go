package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements a DingTalk custom robot.
//
// Platform characteristics (which determine the implementation trade-offs here):
//   - A single robot is rate-limited to 20 messages/minute; excess sends are silently dropped (HTTP may still return 200),
//     so rate limiting must be done on the client side, see DefaultRatePerMin.
//   - Security settings are one of three: signing / custom keywords / IP whitelist. Signing is the only option that doesn't depend on
//     message content, so only signing is supported (bare webhooks with none of the three enabled are also supported).
//   - Both success and failure return HTTP 200, distinguished by the errcode in the body—not checking errcode
//     would record delivery failures as successes.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's Webhook address contains the access_token, which is itself a credential, so mask it entirely.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The target is DingTalk's Webhook address itself; changing the address must also re-declare the signing secret for the new address.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook address")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook address: %w", err)
	}
	return nil
}

// Send delivers a single message. When there is a backlink and it's a single message, use ActionCard (with a button); otherwise use markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown body has no explicit byte limit, but a limit is still enforced to avoid abnormal expansion of evidence fields.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside 200 responses.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is signature verification failure, 310000 is keyword mismatch—both are configuration errors,
		// retrying won't self-heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends timestamp and sign parameters to the webhook per the official signing rules.
//
// Rule: string to sign = timestamp + "\n" + secret, the **key of the HMAC-SHA256 is also secret**,
// the result is base64-encoded then URL-encoded. timestamp is in milliseconds. When secret is empty, return as-is,
// to support robots with signing not enabled.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Don't pass through err: url.Parse's error text contains the full address (including access_token).
		return "", fmt.Errorf("Failed to parse webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL validates that the URL is usable, the scheme is supported, and performs an internal-network check for literal IP targets.
//
// Two things to note:
//
//  1. **Error messages must be redacted**. url.Parse itself returns a *url.Error, whose Error() carries
//     the **full original URL**, and the URLs of these providers embed credentials (DingTalk access_token,
//     WeCom key, Telegram bot token, Feishu hook id). This used to just `return err`,
//     so the 'invalid URL format' error leaked the credentials, flowing into the 400 response of the test endpoint,
//     the last_error stored on every delivery, the server logs, and the delivery history endpoint.
//
//  2. **Literal IPs are directly judged as internal**, domains are left to the dial stage (blockInternalDial is the final
//     enforcement point, and also covers DNS rebinding). Doing it here is so that a hint is available when saving the config,
//     rather than waiting until the first delivery fails.
//
// Restricting schemes is defensive: file:///gopher:// etc. would cause http.Client to behave unexpectedly
// (though already blocked by the scheme check, there is no reason to open up this surface).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL cannot be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
