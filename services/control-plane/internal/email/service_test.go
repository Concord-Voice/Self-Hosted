package email

import (
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDevService() *Service {
	cfg := &config.Config{
		SMTPHost: "", // empty = dev mode
	}
	return NewService(cfg, logger.New("test"))
}

func TestNewServiceDevMode(t *testing.T) {
	svc := newDevService()
	assert.True(t, svc.IsDevMode())
	assert.True(t, svc.devMode)
}

func TestNewServiceProductionMode(t *testing.T) {
	cfg := &config.Config{
		SMTPHost:     "smtp.example.com",
		SMTPPort:     587,
		SMTPUsername: "user",
		SMTPPassword: "pass",
		SMTPFrom:     "noreply@example.com",
	}
	svc := NewService(cfg, logger.New("test"))
	assert.False(t, svc.IsDevMode())
}

func TestSendVerificationCodeDevMode(t *testing.T) {
	svc := newDevService()
	err := svc.SendVerificationCode("test@example.com", "123456")
	assert.NoError(t, err)
}

func TestSendRecoveryCodeDevMode(t *testing.T) {
	svc := newDevService()
	err := svc.SendRecoveryCode("test@example.com", "654321")
	assert.NoError(t, err)
}

func TestSendOwnershipTransferNotificationDevMode(t *testing.T) {
	svc := newDevService()
	err := svc.SendOwnershipTransferNotification(
		"owner@example.com",
		"My Server",
		"newowner",
		"token-abc-123",
	)
	assert.NoError(t, err)
}

func TestSendTemplatedEmailInvalidFrom(t *testing.T) {
	svc := &Service{
		from:    "invalid-email",
		devMode: false,
	}
	err := svc.sendTemplatedEmail("to@example.com", "Test", verificationTmpl, map[string]string{"Code": "123456"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid from address")
}

func TestSendTemplatedEmailInvalidTo(t *testing.T) {
	svc := &Service{
		from:    "valid@example.com",
		devMode: false,
	}
	err := svc.sendTemplatedEmail("invalid-email", "Test", verificationTmpl, map[string]string{"Code": "123456"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid to address")
}

func TestTemplatesLoad(t *testing.T) {
	// Verify embedded templates loaded correctly
	assert.NotNil(t, verificationTmpl)
	assert.NotNil(t, recoveryTmpl)
	assert.NotNil(t, ownershipTransferTmpl)
}
