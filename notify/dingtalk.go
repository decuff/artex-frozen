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

// dingTalkChannel implements the DingTalk custom robot.
//
// Platform characteristics (which determine the implementation choices here):
//   - A single robot is rate-limited to 20 messages/minute; excess messages are silently dropped (HTTP may still return 200),
//     so rate limiting must be done on the client side, see DefaultRatePerMin.
//   - One of three security settings: signing / custom keywords / IP allowlist. Signing is the only option that does not depend on
//     message content, so only signing is supported (a bare webhook with none of the three enabled is also supported).
//   - Both success and failure return HTTP 200, distinguished by the errcode in the body—not checking errcode
//     would record delivery failures as successes.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// The DingTalk Webhook URL contains an access_token, which is itself a credential, so mask the whole thing.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The target is the DingTalk Webhook URL itself; changing the URL must also re-enter the signing secret for the new URL.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Invalid Webhook URL: %w", err)
	}
	return nil
}

// Send delivers a message once. If there is a backlink and it is a single message, use ActionCard (with a button); otherwise use markdown.
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
	// The DingTalk markdown body has no clear byte limit, but a limit is still enforced to prevent evidence fields from growing abnormally large.
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
		return 0, fmt.Errorf("Failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is signature verification failure, 310000 is keyword mismatch—both are configuration errors,
		// and retrying will not self-heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends timestamp and sign parameters to the webhook per the official signing rules.
//
// Rules: string to sign = timestamp + "\n" + secret, and the HMAC-SHA256 **key is also the secret**,
// the result is base64-encoded then URL-encoded. timestamp is in milliseconds. When secret is empty, return as-is,
// to support bots without signing enabled.
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
		// Do not pass through err: the error text from url.Parse carries the full URL (including access_token).
		return "", fmt.Errorf("Failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL verifies the URL is usable and its scheme is supported, and checks literal IP targets against internal networks.
//
// Two subtleties:
//
//  1. **Error messages must be redacted**. url.Parse itself returns a *url.Error whose Error() carries
//     **the full original URL**, and the URLs of these providers for this feature embed credentials (DingTalk access_token,
//     WeCom key, Telegram bot token, Feishu hook id). This used to `return err` directly,
//     so the "invalid URL format" error leaked the credentials into the test interface's 400 response,
//     each delivery's persisted last_error, server logs, and the delivery history API.
//
//  2. **Literal IPs are judged internal directly**, domains are left for the dial stage (blockInternalDial is the final
//     enforcement point and also covers DNS rebinding). Doing it here lets users get a hint when saving the configuration,
//     rather than waiting for the first delivery to fail.
//
// Restricting the scheme is defensive: file:///gopher:// and the like cause http.Client to behave unexpectedly
// (though already blocked by the scheme check, there is no reason to open up this surface).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL cannot be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("Only http/https are supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("Missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("Refusing to deliver to localhost/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
