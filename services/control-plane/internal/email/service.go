// Package email provides email sending functionality for user verification and notifications.
package email

import (
	"crypto/tls"
	"embed"
	"fmt"
	"html/template"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

//go:embed templates/*.html
var templateFS embed.FS

var verificationTmpl = template.Must(template.ParseFS(templateFS, "templates/verification.html"))

// Service sends emails via SMTP. When SMTPHost is empty (dev mode), codes are
// logged to stdout instead of sent.
type Service struct {
	host     string
	port     int
	username string
	password string // #nosec G101 -- struct field name, not a secret
	from     string
	log      *logger.Logger
	devMode  bool
}

// NewService creates an email service from the application config.
func NewService(cfg *config.Config, log *logger.Logger) *Service {
	return &Service{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		username: cfg.SMTPUsername,
		password: cfg.SMTPPassword, // #nosec G101 -- loaded from env, not hardcoded
		from:     cfg.SMTPFrom,
		log:      log,
		devMode:  cfg.SMTPHost == "",
	}
}

// SendVerificationCode sends a 6-digit verification code to the given email address.
func (s *Service) SendVerificationCode(to, code string) error {
	if s.devMode {
		s.log.Info("DEV MODE — email verification code", "to", to, "code", code)
		return nil
	}

	// Render HTML body
	var body strings.Builder
	err := verificationTmpl.Execute(&body, map[string]string{
		"Code": code,
	})
	if err != nil {
		return fmt.Errorf("render email template: %w", err)
	}

	// Validate and parse email addresses to prevent header/envelope injection
	fromAddr, err := mail.ParseAddress(s.from)
	if err != nil {
		return fmt.Errorf("invalid from address: %w", err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("invalid to address: %w", err)
	}

	// Build MIME message using parsed addresses
	msg := "From: " + fromAddr.String() + "\r\n" +
		"To: " + toAddr.String() + "\r\n" +
		"Subject: Your Concord verification code\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body.String()

	addr := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))

	// Connect with STARTTLS
	auth := smtp.PlainAuth("", s.username, s.password, s.host)
	tlsCfg := &tls.Config{ServerName: s.host} // #nosec G402 -- using server name verification

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial SMTP: %w", err)
	}

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close() // prevent socket leak if NewClient fails
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Require STARTTLS in production (verification codes are sensitive even without credentials)
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	} else {
		return fmt.Errorf("SMTP server does not support STARTTLS — refusing to send verification codes over plaintext")
	}

	// Authenticate if credentials are provided
	if s.username != "" {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	// Send using validated addresses for envelope
	if err := client.Mail(fromAddr.Address); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(toAddr.Address); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("SMTP write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close data: %w", err)
	}

	return client.Quit()
}

// IsDevMode returns true when the service is operating without a real SMTP server.
func (s *Service) IsDevMode() bool {
	return s.devMode
}
