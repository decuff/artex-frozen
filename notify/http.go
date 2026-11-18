package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets determines whether messages may be delivered to loopback / link-local addresses.
//
// Default deny. These address ranges are not where IM bots or public mail servers would appear, yet they can
// What it hits is very sensitive: the admin port of another service on the same machine, and the metadata endpoint of the cloud environment
// (169.254.169.254, instance credentials can be read out). The delivery address is configured by the admin, but one that is XSS/CSRF
// A borrowed admin session, or a second person sharing the same JWT, can read the response content back by changing the config
// ——doJSON writes the first 200 bytes of the 4xx/5xx response body into last_error, while the delivery history endpoint
// It will echo it back, which is a semi-blind read primitive.
//
// But a 'local SMTP relay' (postfix on 127.0.0.1:25) is a common setup for self-hosted mail,
// A one-size-fits-all rule would get people stuck. So leave an explicit escape hatch instead of hardcoding an exemption:
// Set ARTEX_NOTIFY_ALLOW_LOCAL=1 to allow it.
//
// Exported as AllowLocalTargetsEnv so tests can explicitly enable it—this package and the server package's
// The test cases heavily use httptest fake receivers on 127.0.0.1; if not enabled, they are all blocked by the guard.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP belongs to an address range that is 'not allowed to be dialed by default'.
//
// Only reject loopback, link-local (including cloud metadata 169.254.169.254), unspecified, and multicast.
// **Do not reject** RFC1918 private networks: self-hosted Mattermost / SMTP relay on the intranet is a very common legitimate use case,
// Blocking them all together would make the feature directly unusable in a real environment. This trade-off is deliberate—
// The protection must block the truly sensitive targets while not breaking normal deployments along with it.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be restored to IPv4 before checking, otherwise the check is bypassed.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook of the http.Transport dialer, at **connection establishment time**
// Check the target address.
//
// Why set it at the dialing stage instead of only validating when saving the config: this is the point where it finally takes effect.
// It also covers both cases of bypassing configuration validation—DNS rebinding (resolving to a public IP during validation,
// resolved to the internal network on the actual connection) and redirects (although we already reject cross-host redirects, same-host redirects
// It may still point the path elsewhere).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("unable to parse target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds only a dial guard on top of the default Transport.
// Use Clone to keep all the default tuning (connection pool, HTTP/2, timeouts, proxy, etc.),
// avoiding changes to other behavior just to add one check.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by all channel deliveries.
//
// Deliberately **does not** reuse the project's global egress proxy (the server-side GlobalProxy): that proxy is for pentest
// target traffic and is often an unstable tunnel, while notification availability should not be held hostage by target network jitter.
// IM push can connect directly. Timeout is set to 15 seconds—a peer slower than this is effectively already in a failure state.
//
// Reject cross-host redirects: the delivery addresses for this feature are all in the form of 'a fixed endpoint', so normally they won't
// redirect to another host; and the credentials of these providers (DingTalk's access_token, WeCom's key, Telegram's
// bot token) are **right in the URL**, so following a cross-host redirect is equivalent to handing the credentials to the redirect target. Same-host
// redirects (such as appending a trailing slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit limits the size of the response body read. When the peer is abnormal it may return huge content, while we only need
// the error code and a short error description for display in the delivery history.
const respBodyLimit = 8 << 10

// doJSON sends a request once and returns the response body (length-limited).
//
// When payload is nil, sends an empty body (for GET or scenarios where the platform does not require a body).
// Key-value pairs in headers are appended as-is, for custom headers of generic Webhooks.
//
// Error classification is the core responsibility of this function: network-layer failures and 5xx/408/429 are classified as 'retryable',
// while other 4xx are classified as 'permanent failure'—retrying a 403 just spams the same error into the log 3 times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failure is a local bug (wrong config field type), and retrying won't make it better.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Invalid URL—most likely the user filled in the address wrong, which is a permanent failure.
		// Here too err must not be passed through: the error text of url.Parse contains the full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout—mostly transient faults, hand off to backoff retry.
		//
		// Error text must be redacted before being passed outward. Reason: http.Client.Do returns *url.Error,
		// whose Error() is `Op "full URL": underlying error`, and for these providers in this feature the credentials are **right in the URL**
		// (DingTalk access_token, WeCom key, Feishu hook id, Telegram /bot<token>/).
		// Without redaction, credentials would flow along this error string to four places: notification_deliveries
		// last_error (stored in plaintext in the DB), the delivery history API response (**bypassing the channel config masking**),
		// server logs, and the 502 text returned to the frontend by the test-send API.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limit) and 408 (timeout) are worth retrying; other 4xx are config or permission issues, retrying is meaningless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("peer rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("peer service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("peer rejected request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses the response body into a single short line of text, for error messages. The response may contain newlines and lots of whitespace,
// stuffing it directly into last_error would break the layout of the delivery history page.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses the delivery address into 'scheme://host/…', for error messages.
//
// This is the only address-redaction convention in this package, deliberately made **crude enough**: except for scheme and host,
// everything else is discarded. Reason: there is no 'universal and safe' way to determine which part of a URL is a credential:
//
//	DingTalk   credential in query      /robot/send?access_token=xxx
//	WeCom   credential in query      /cgi-bin/webhook/send?key=xxx
//	Feishu   credential in **last path segment** /open-apis/bot/v2/hook/<hook_id>
//	Telegram credential in **middle path segment** /bot<token>/sendMessage
//
// To 'keep only the useful part' you'd have to patch per channel, and missing any one is a credential leak.
// Keeping host is already enough for troubleshooting (DNS resolution failure, can't connect, bad certificate can all be located),
// Which specific bot it is is identified by the masked tail number hint in the channel config.
//
// On parse failure, return a fixed placeholder — never echo the raw string back out.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address unparseable)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address from a transport-layer error, keeping only the underlying cause.
//
// The structure of *url.Error is {Op, URL, Err}, and Error() prints the URL along with it.
// Here we explicitly take the Err field, bypassing its Error() — more reliable than doing string replacement after the fact,
// because replacement must correctly handle all the URL-encoded/escaped variants, which is easy to miss.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error (such as errors returned by redirect policies) may also carry an address, so route them all through redaction.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces http(s) addresses appearing in a piece of text with their redacted form.
//
// Used to catch errors where structured fields are unavailable (redirect policy errors, custom errors from third-party libraries).
// Only recognizes the http/https prefix, splitting on whitespace and quotes — addresses won't contain these two kinds of characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
