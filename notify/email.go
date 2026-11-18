package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout constrain connection establishment and the entire SMTP session, respectively.
// net/smtp itself has no timeout mechanism; without these two, a stuck peer would leave
// the delivery goroutine hanging forever—and the dispatcher processes serially in a single goroutine,
// meaning the entire notification system would stall.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but it shouldn't be used to spam; give it a lenient default value.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only mask the password. SMTP host, account, and recipients aren't secrets; masking them only makes editing cumbersome.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port determine which server the password is sent to; tls determines whether transmission is encrypted. Any change to these three
// requires re-confirming the password—which also forces 'disabling TLS' to explicitly carry credentials, rather than being a casual change.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("missing SMTP server address")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("invalid SMTP port (should be 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("missing sender address")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("at least one recipient address is required")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: upgrade if the peer supports it. Credentials must not be sent over a plaintext session (see the auth note below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over an unencrypted connection (unless the target is localhost).
			// This is **correct** security behavior and must not be bypassed, but the reason needs to be translated clearly—
			// otherwise users will only see 'unencrypted connection' and not know what to do.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials: connection is not encrypted. Please enable TLS, or use port 465 (implicit TLS), or check 'Enable TLS' (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP authentication failed: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("sender %s rejected", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("recipient %s rejected", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("failed to write email body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("failed to submit email: %w", err)
	}
	// A failed Quit does not affect the fact that 'the mail has been received by the server', so its error is ignored.
	_ = client.Quit()
	// The mail is not truncated in length (the entire HTML body is sent), so the whole batch counts as delivered.
	return len(m.Items), nil
}

// emailDial establishes an SMTP connection.
//
// implicitTLS=true uses the 'TLS immediately on connect' style like port 465; false uses plaintext connect on 25/587 followed by
// STARTTLS. The two cannot be mixed: sending a plaintext greeting to port 465 will be disconnected immediately.
//
// The session deadline is set at the **connection point** (rather than patched in afterward), because net/smtp's Client hides the underlying
// connection in an unexported field, and the outside cannot access it; once the connection is handed over, only the pre-set deadline
// can serve as a fallback. This also incidentally covers blocking during the handshake phase.
// Control attaches blockInternalDial, sharing the same guard as the HTTP-family channels. Without it, SMTP
// would be a gap in the entire SSRF protection: a host set to 169.254.169.254 or 127.0.0.1 can connect directly,
// and when smtp.NewClient's handshake fails, it wraps the line returned by the peer into the error, which via last_error
// is echoed back by the delivery history API, forming a semi-blind read primitive; the timing difference between 'connection refused vs timeout' can also
// be used to probe ports. The dial phase is the final effective point, and also covers DNS rebinding.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError classifies a stage's failure into 'retryable' and 'permanent failure' based on the SMTP reply code.
//
// Why the distinction is necessary: SMTP's 4xx and 5xx have completely different semantics——
//   - 4xx (450 greylisting, 451 local error, 452 insufficient storage) are **temporary** rejections,
//     the proper approach is to retry later; especially greylisting, which is encountered on almost every first delivery.
//   - 5xx (550 user does not exist, 553 illegal address) are permanent rejections, and retrying is meaningless.
//
// If everything were judged as permanent failure, a mail server with greylisting enabled would make **every** push fall into
// failed after the first attempt——and this kind of failure is exactly the scenario where automatic retry should play its greatest role.
// The reply code is taken from the first three digits of the error text; when no code can be obtained, treat it as retryable (better to try once more
// than to declare a possible transient fault dead just because it cannot be parsed).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode takes the leading three-digit reply code from the SMTP error text, returning 0 if it cannot be obtained.
// net/smtp does not export error code fields, so they can only be extracted from the text; the format is '450 4.7.1 ...'.
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage assembles a complete RFC 5322 email.
//
// The body is base64-encoded for two reasons: first, SMTP specifies that a single line must not exceed 1000 bytes, and HTML
// bodies (especially summary emails) easily produce overly long lines; second, base64 naturally never produces lines starting with "."
// so it saves the trouble of SMTP dot-stuffing.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// Chinese subjects must be RFC 2047 encoded, otherwise they will be displayed as garbled text by clients.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Emails have no hard length limit, so the body is not truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 wraps lines at 76 characters, in accordance with RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
