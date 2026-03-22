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
var recoveryTmpl = template.Must(template.ParseFS(templateFS, "templates/recovery.html"))
var ownershipTransferTmpl = template.Must(template.ParseFS(templateFS, "templates/ownership_transfer.html"))

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

	var body strings.Builder
	err := verificationTmpl.Execute(&body, map[string]string{
		"Code": code,
	})
	if err != nil {
		return fmt.Errorf("render email template: %w", err)
	}

	fromAddr, err := mail.ParseAddress(s.from)
	if err != nil {
		return fmt.Errorf("invalid from address: %w", err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("invalid to address: %w", err)
	}

	msg := "From: " + fromAddr.String() + "\r\n" +
		"To: " + toAddr.String() + "\r\n" +
		"Subject: Your Concord verification code\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body.String()

	return s.sendMail(fromAddr.Address, toAddr.Address, msg)
}

// SendRecoveryCode sends a 6-digit account recovery code to the given email address.
func (s *Service) SendRecoveryCode(to, code string) error {
	if s.devMode {
		s.log.Info("DEV MODE — account recovery code", "to", to, "code", code)
		return nil
	}

	var body strings.Builder
	err := recoveryTmpl.Execute(&body, map[string]string{
		"Code": code,
	})
	if err != nil {
		return fmt.Errorf("render recovery template: %w", err)
	}

	fromAddr, err := mail.ParseAddress(s.from)
	if err != nil {
		return fmt.Errorf("invalid from address: %w", err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("invalid to address: %w", err)
	}

	msg := "From: " + fromAddr.String() + "\r\n" +
		"To: " + toAddr.String() + "\r\n" +
		"Subject: Your Concord account recovery code\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body.String()

	return s.sendMail(fromAddr.Address, toAddr.Address, msg)
}

// sendMail handles the SMTP connection and message delivery.
func (s *Service) sendMail(from, to, msg string) error {
	addr := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))

	auth := smtp.PlainAuth("", s.username, s.password, s.host)
	tlsCfg := &tls.Config{ServerName: s.host} // #nosec G402 -- using server name verification

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial SMTP: %w", err)
	}

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	} else {
		return fmt.Errorf("SMTP server does not support STARTTLS — refusing to send codes over plaintext")
	}

	if s.username != "" {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
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

// SendOwnershipTransferNotification sends an ownership transfer notification to the server owner.
func (s *Service) SendOwnershipTransferNotification(to, serverName, newOwnerUsername, reversalToken string) error {
	if s.devMode {
		s.log.Info("DEV MODE — ownership transfer notification",
			"to", to, "server", serverName, "new_owner", newOwnerUsername, "reversal_token", reversalToken)
		return nil
	}

	var body strings.Builder
	err := ownershipTransferTmpl.Execute(&body, map[string]string{
		"ServerName":       serverName,
		"NewOwnerUsername": newOwnerUsername,
		"ReversalToken":    reversalToken,
	})
	if err != nil {
		return fmt.Errorf("render ownership transfer template: %w", err)
	}

	fromAddr, err := mail.ParseAddress(s.from)
	if err != nil {
		return fmt.Errorf("invalid from address: %w", err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("invalid to address: %w", err)
	}

	// Sanitize server name to prevent SMTP header injection
	safeServerName := strings.NewReplacer("\r", "", "\n", "").Replace(serverName)

	msg := "From: " + fromAddr.String() + "\r\n" +
		"To: " + toAddr.String() + "\r\n" +
		"Subject: Ownership transfer initiated for " + safeServerName + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body.String()

	return s.sendMail(fromAddr.Address, toAddr.Address, msg)
}

// IsDevMode returns true when the service is operating without a real SMTP server.
func (s *Service) IsDevMode() bool {
	return s.devMode
}
