package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// Brand is the copy shared by every outgoing email: who it is from, where to
// get help, and the links that carry single-use tokens back into the app.
//
// It is passed in rather than read from config so this package stays free of a
// dependency on the process's configuration.
type Brand struct {
	AppName      string
	SupportEmail string
	// PasswordResetURL and EmailVerifyURL are the base links the token is
	// appended to, typically an app deep link. When empty the mail carries the
	// code on its own, which the app's "I have a code" screen accepts; that is
	// the honest fallback rather than a link that goes nowhere.
	PasswordResetURL string
	EmailVerifyURL   string
}

func (b Brand) withDefaults() Brand {
	if strings.TrimSpace(b.AppName) == "" {
		b.AppName = "MatchUp"
	}
	return b
}

// tokenLink appends the token to a base link, preserving any query string the
// base already carries.
func tokenLink(base, token string) string {
	base = strings.TrimSpace(base)
	if base == "" || token == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}

// emailBody is what the layout renders around. Every field is escaped by
// html/template, which matters because Name is user-supplied.
type emailBody struct {
	AppName      string
	Heading      string
	Greeting     string
	Paragraphs   []string
	ButtonLabel  string
	ButtonURL    string
	Code         string
	Footer       string
	SupportEmail string
}

// layoutHTML is deliberately plain: inline styles, one column, no images and no
// external assets, which is what survives Gmail, Outlook and the Ethiopian
// webmail clients alike.
var layoutHTML = template.Must(template.New("email").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Heading}}</title></head>
<body style="margin:0;padding:24px;background:#f4f5f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1f2430;">
  <div role="article" aria-label="{{.Heading}}" style="max-width:560px;margin:0 auto;background:#ffffff;border-radius:12px;padding:32px;">
    <p style="margin:0 0 24px;font-size:14px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:#5b6472;">{{.AppName}}</p>
    <h1 style="margin:0 0 16px;font-size:22px;line-height:1.3;">{{.Heading}}</h1>
    {{if .Greeting}}<p style="margin:0 0 16px;font-size:16px;line-height:1.6;">{{.Greeting}}</p>{{end}}
    {{range .Paragraphs}}<p style="margin:0 0 16px;font-size:16px;line-height:1.6;">{{.}}</p>{{end}}
    {{if .ButtonURL}}<p style="margin:24px 0;"><a href="{{.ButtonURL}}" style="display:inline-block;background:#0f7b4f;color:#ffffff;text-decoration:none;font-size:16px;font-weight:600;padding:12px 24px;border-radius:8px;">{{.ButtonLabel}}</a></p>{{end}}
    {{if .Code}}<p style="margin:0 0 8px;font-size:14px;color:#5b6472;">Or enter this code in the app:</p>
    <p style="margin:0 0 24px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:20px;letter-spacing:.04em;background:#f4f5f7;border-radius:8px;padding:12px 16px;word-break:break-all;">{{.Code}}</p>{{end}}
    {{if .Footer}}<p style="margin:24px 0 0;font-size:14px;line-height:1.6;color:#5b6472;">{{.Footer}}</p>{{end}}
    {{if .SupportEmail}}<p style="margin:16px 0 0;font-size:14px;color:#5b6472;">Need help? Reply to this email or write to <a href="mailto:{{.SupportEmail}}" style="color:#0f7b4f;">{{.SupportEmail}}</a>.</p>{{end}}
  </div>
</body>
</html>`))

func (b emailBody) html() string {
	var buf bytes.Buffer
	if err := layoutHTML.Execute(&buf, b); err != nil {
		// Execution can only fail on a template bug, and a broken layout must
		// not stop the mail: the text part still carries everything.
		return "<pre>" + template.HTMLEscapeString(b.text()) + "</pre>"
	}
	return buf.String()
}

// text is the plain-text alternative. Some clients show it, and spam filters
// score a multipart mail better than an HTML-only one.
func (b emailBody) text() string {
	var sb strings.Builder
	sb.WriteString(b.Heading + "\n\n")
	if b.Greeting != "" {
		sb.WriteString(b.Greeting + "\n\n")
	}
	for _, p := range b.Paragraphs {
		sb.WriteString(p + "\n\n")
	}
	if b.ButtonURL != "" {
		sb.WriteString(b.ButtonLabel + ": " + b.ButtonURL + "\n\n")
	}
	if b.Code != "" {
		sb.WriteString("Code: " + b.Code + "\n\n")
	}
	if b.Footer != "" {
		sb.WriteString(b.Footer + "\n\n")
	}
	if b.SupportEmail != "" {
		sb.WriteString("Need help? Write to " + b.SupportEmail + "\n")
	}
	return sb.String()
}

// greeting addresses the recipient by first name when one is known.
func greeting(fullName string) string {
	name := strings.TrimSpace(fullName)
	if name == "" {
		return "Hello,"
	}
	if first, _, found := strings.Cut(name, " "); found && first != "" {
		name = first
	}
	return "Hello " + name + ","
}

// notificationEmail renders a queued notification as an email, reusing the same
// title and body the push carries so the two channels never disagree.
func (b Brand) notificationEmail(fullName string, n db.Notification) emailBody {
	brand := b.withDefaults()
	return emailBody{
		AppName:      brand.AppName,
		Heading:      n.Title,
		Greeting:     greeting(fullName),
		Paragraphs:   []string{n.Body},
		Footer:       "You are receiving this because it concerns a booking on your account.",
		SupportEmail: brand.SupportEmail,
	}
}

// expiryPhrase turns the token's expiry into "in 30 minutes" style copy, since
// a timestamp in a foreign timezone tells the reader nothing useful.
func expiryPhrase(raw string, now time.Time) string {
	if raw == "" {
		return ""
	}
	expires, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	left := expires.Sub(now).Round(time.Minute)
	switch {
	case left <= 0:
		return ""
	case left < time.Hour:
		return fmt.Sprintf("This link expires in %d minutes.", int(left.Minutes()))
	case left < 24*time.Hour:
		hours := int(left.Hours())
		if hours == 1 {
			return "This link expires in an hour."
		}
		return fmt.Sprintf("This link expires in %d hours.", hours)
	default:
		return "This link expires soon, so use it while it is fresh."
	}
}

// renderOutbox turns one email_outbox row into a sendable message.
func (b Brand) renderOutbox(row db.EmailOutbox, now time.Time) (EmailMessage, error) {
	brand := b.withDefaults()

	var p payload
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return EmailMessage{}, fmt.Errorf("%w: decode email payload: %v", ErrEmailRejected, err)
		}
	}
	token := p.str("token")
	expiry := expiryPhrase(p.str("expires_at"), now)

	var subject string
	body := emailBody{
		AppName:      brand.AppName,
		Greeting:     greeting(p.str("full_name")),
		Code:         token,
		SupportEmail: brand.SupportEmail,
	}

	switch row.Template {
	case TemplatePasswordReset:
		subject = "Reset your " + brand.AppName + " password"
		body.Heading = "Reset your password"
		body.ButtonLabel = "Choose a new password"
		body.ButtonURL = tokenLink(brand.PasswordResetURL, token)
		body.Paragraphs = []string{
			"Someone asked to reset the password on your " + brand.AppName + " account.",
		}
		if expiry != "" {
			body.Paragraphs = append(body.Paragraphs, expiry)
		}
		// Named explicitly: a reset mail arriving unrequested is the one
		// signal a user has that someone is trying their address.
		body.Footer = "If this was not you, you can ignore this email. Your password stays as it is, and this link can only be used once."

	case TemplateEmailVerification:
		subject = "Confirm your email address"
		body.Heading = "Confirm your email address"
		body.ButtonLabel = "Confirm my email"
		body.ButtonURL = tokenLink(brand.EmailVerifyURL, token)
		body.Paragraphs = []string{
			"Confirming your address lets us reach you about your bookings and payments.",
		}
		if expiry != "" {
			body.Paragraphs = append(body.Paragraphs, expiry)
		}
		body.Footer = "If you did not create a " + brand.AppName + " account, you can ignore this email."

	default:
		return EmailMessage{}, fmt.Errorf("%w: no template for %q", ErrEmailRejected, row.Template)
	}

	if body.ButtonURL == "" && token == "" {
		// Neither a link nor a code means the mail cannot do its job; sending
		// it would only confuse the recipient.
		return EmailMessage{}, fmt.Errorf("%w: email %q has neither link nor token", ErrEmailRejected, row.Template)
	}

	return EmailMessage{
		To:      row.ToAddress,
		Subject: subject,
		HTML:    body.html(),
		Text:    body.text(),
	}, nil
}

// DispatchEmails claims a batch of queued transactional emails and sends them,
// returning how many were claimed.
//
// With no sender configured the rows are left untouched rather than claimed and
// failed, so configuring Resend later drains the backlog instead of discarding
// it.
func (d *Dispatcher) DispatchEmails(ctx context.Context) (int, error) {
	if d.email == nil {
		return 0, nil
	}
	claimed, err := d.store.ClaimEmails(ctx, d.batchSize)
	if err != nil {
		return 0, fmt.Errorf("claim emails: %w", err)
	}

	for _, row := range claimed {
		msg, err := d.brand.renderOutbox(row, time.Now())
		if err == nil {
			err = d.email.Send(ctx, msg)
		}
		if err != nil {
			d.rescheduleEmail(ctx, row, err)
			continue
		}
		// Marking sent also scrubs the payload, so a delivered reset mail
		// leaves no live token sitting in the table.
		if err := d.store.MarkEmailSent(ctx, row.ID); err != nil {
			d.log.Error("could not mark email sent",
				slog.String("email_id", row.ID.String()), slog.Any("error", err))
		}
	}
	return len(claimed), nil
}

func (d *Dispatcher) rescheduleEmail(ctx context.Context, row db.EmailOutbox, cause error) {
	message := cause.Error()
	if errors.Is(cause, ErrEmailRejected) {
		// Retrying cannot change the outcome, so the row is failed with the
		// reason intact instead of spending its attempt budget.
		d.log.Warn("email permanently rejected",
			slog.String("email_id", row.ID.String()),
			slog.String("template", row.Template),
			slog.Any("error", cause))
		if err := d.store.FailEmail(ctx, db.FailEmailParams{ID: row.ID, LastError: &message}); err != nil {
			d.log.Error("could not fail email",
				slog.String("email_id", row.ID.String()), slog.Any("error", err))
		}
		return
	}
	d.log.Warn("email send failed",
		slog.String("email_id", row.ID.String()),
		slog.String("template", row.Template),
		slog.Int("attempts", int(row.Attempts)),
		slog.Any("error", cause))
	if err := d.store.RescheduleEmail(ctx, db.RescheduleEmailParams{
		ID:        row.ID,
		Backoff:   db.Interval(Backoff(int(row.Attempts))),
		LastError: &message,
	}); err != nil {
		d.log.Error("could not reschedule email",
			slog.String("email_id", row.ID.String()), slog.Any("error", err))
	}
}
