package api

import (
	"bytes"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestConfigureTrustedProxiesDoesNotLogPeerAddresses(t *testing.T) {
	var output bytes.Buffer
	configureTrustedProxies(gin.New(), &config.Config{TrustedProxyCIDRs: []string{"192.0.2.91/32"}}, logger.NewWithWriter(&output))
	assert.Contains(t, output.String(), "count=1")
	assert.NotContains(t, output.String(), "192.0.2.91")
	assert.NotContains(t, output.String(), "cidrs=")
}
