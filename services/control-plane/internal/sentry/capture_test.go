package sentry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCaptureWithTag_DoesNotPanic(t *testing.T) {
	// Sentry is not initialized in tests — CaptureWithTag must not panic.
	assert.NotPanics(t, func() {
		CaptureWithTag(errors.New("test error"), "worker.job", "cleanup.tokens")
	})
}

func TestCaptureFromContext_DoesNotPanicWithNilContext(t *testing.T) {
	// nil gin.Context — fallback to global hub, must not panic.
	assert.NotPanics(t, func() {
		CaptureFromContext(nil, errors.New("test"), "http.route", "/api/v1/servers")
	})
}
