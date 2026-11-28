package reservationquota_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/reservationquota"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

func TestIsExhaustedMatchesOnlyNamedQuotaViolations(t *testing.T) {
	for _, constraint := range []string{
		"message_id_reservation_global_quota",
		"message_id_reservation_account_quota",
	} {
		t.Run(constraint, func(t *testing.T) {
			err := fmt.Errorf("save: %w", &pq.Error{Code: "23514", Constraint: constraint})
			assert.True(t, reservationquota.IsExhausted(err))
		})
	}
	assert.False(t, reservationquota.IsExhausted(&pq.Error{
		Code: "23514", Constraint: "message_id_reservations_message_id_immutable",
	}))
	assert.False(t, reservationquota.IsExhausted(&pq.Error{
		Code: "23505", Constraint: "message_id_reservation_global_quota",
	}))
	assert.False(t, reservationquota.IsExhausted(errors.New("message ID quota")))
}
