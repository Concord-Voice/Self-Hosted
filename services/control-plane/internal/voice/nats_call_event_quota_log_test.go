package voice

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

func TestDMCallEventFailureLogsCapacityDistinctly(t *testing.T) {
	var output bytes.Buffer
	s := &NATSSubscriber{log: logger.NewWithWriter(&output)}
	s.logDMCallEventFailure(&pq.Error{
		Code: "23514", Constraint: "message_id_reservation_account_quota",
	})
	assert.Contains(t, output.String(), "level=WARN")
	assert.Contains(t, output.String(), "failure_class=capacity")
	assert.NotContains(t, output.String(), "failure_class=state_write")

	output.Reset()
	s.logDMCallEventFailure(errors.New("database unavailable"))
	assert.Contains(t, output.String(), "level=ERROR")
	assert.Contains(t, output.String(), "failure_class=state_write")
}
