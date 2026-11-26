// Package mail is iterion's minimal transactional mailer: stdlib SMTP
// (explicit STARTTLS) + embedded templates for the two flows that
// need email — invitations and password resets. No third-party
// dependency by design.
//
// When SMTP isn't configured the LogMailer stands in: flows still
// mint their tokens and the would-be email lands in the logs (handy
// for local dev and tests). The server exposes `email_enabled` on
// /api/server/info so the SPA only offers email-dependent flows
// (forgot-password) when a real mailer is wired.
package mail

import (
	"context"
	"fmt"
	"mime"
	"strings"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Message is one outbound transactional email. Both bodies are
// provided; the MIME envelope is multipart/alternative.
type Message struct {
	To       string
	Subject  string
	TextBody string
	HTMLBody string
}

// Mailer sends transactional email.
type Mailer interface {
	Send(ctx context.Context, m Message) error
	// Enabled reports whether sends actually leave the process —
	// false for the log fallback. Drives `email_enabled` in
	// server_info.
	Enabled() bool
}

// Config is the SMTP transport configuration (ITERION_SMTP_* env in
// the server bootstrap; helm `config.smtp.*` + `secrets.smtp`).
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	// From is the envelope + header sender, e.g. "iterion <no-reply@example.org>".
	From string
	// StartTLS upgrades the connection before AUTH (default true).
	// Disabling it is only sane against a localhost relay.
	StartTLS bool
	// Timeout bounds the whole SMTP conversation (default 10s).
	Timeout time.Duration
	// Logger records what does not fail a send: a QUIT refused after
	// the relay took the message. Nil logs nothing.
	Logger *iterlog.Logger
}

// buildMIME assembles a multipart/alternative envelope. Header values
// are sanitised against CRLF injection; the subject is RFC 2047
// encoded so non-ASCII survives.
func buildMIME(from string, m Message) []byte {
	const boundary = "iterion-mail-boundary"
	clean := func(s string) string {
		return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", clean(from))
	fmt.Fprintf(&b, "To: %s\r\n", clean(m.To))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", clean(m.Subject)))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", boundary)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(m.TextBody)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString(m.HTMLBody)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}
