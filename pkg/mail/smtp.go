package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"time"
)

// SMTPMailer sends via stdlib SMTP with an explicit STARTTLS upgrade.
// Deliberately NOT smtp.SendMail: the wrapper silently skips AUTH on
// a plaintext session, which would post credentials-less mail or leak
// nothing at all depending on the relay — explicit client calls make
// the TLS-before-AUTH ordering auditable.
type SMTPMailer struct{ cfg Config }

// NewSMTP validates the config and returns the mailer.
func NewSMTP(cfg Config) (*SMTPMailer, error) {
	if cfg.Host == "" {
		return nil, errors.New("mail: smtp host required")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.From == "" {
		return nil, errors.New("mail: from address required")
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("mail: invalid from address: %w", err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &SMTPMailer{cfg: cfg}, nil
}

func (s *SMTPMailer) Enabled() bool { return true }

func (s *SMTPMailer) Send(ctx context.Context, m Message) error {
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("mail: invalid recipient: %w", err)
	}
	from, _ := mail.ParseAddress(s.cfg.From)

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	d := net.Dialer{Timeout: s.cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	// One deadline bounds the whole conversation — a hung relay can't
	// pin the goroutine past the configured timeout.
	deadline := time.Now().Add(s.cfg.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: smtp handshake: %w", err)
	}
	defer c.Close()

	if s.cfg.StartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("mail: server does not advertise STARTTLS (set startTLS=false only against a trusted local relay)")
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return fmt.Errorf("mail: starttls: %w", err)
		}
	}
	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := wc.Write(buildMIME(s.cfg.From, m)); err != nil {
		wc.Close()
		return fmt.Errorf("mail: write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("mail: finalize: %w", err)
	}
	// The relay answered 250 to the end of DATA: it has taken the
	// message. A QUIT that fails now (dropped connection, EOF) changes
	// nothing for the recipient, so the send stands and the deferred
	// Close releases the connection.
	if err := c.Quit(); err != nil {
		s.cfg.Logger.Warn("mail: %s took the message, then QUIT failed: %v", addr, err)
	}
	return nil
}
