// Package reservationquota identifies the database's durable message-ID
// issuance limit without exposing which limit was reached to clients.
package reservationquota

import (
	"errors"

	"github.com/lib/pq"
)

const (
	// Code is the client-visible reason for either durable issuance ceiling.
	Code = "message_storage_capacity"
	// Message is the shared user-facing capacity refusal.
	Message = "Message storage is at capacity"
)

// IsExhausted matches only the two quota constraints. Other check violations
// retain their existing error handling, including E2EE and retention checks.
func IsExhausted(err error) bool {
	var dbErr *pq.Error
	if !errors.As(err, &dbErr) || dbErr.Code != "23514" {
		return false
	}
	return dbErr.Constraint == "message_id_reservation_global_quota" ||
		dbErr.Constraint == "message_id_reservation_account_quota"
}
