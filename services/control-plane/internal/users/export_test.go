package users

import "github.com/google/uuid"

// CustomTextResetBroadcasterForTest is the test-only destructive-reset clear
// seam used to verify post-commit delivery ordering.
type CustomTextResetBroadcasterForTest interface {
	ClearCustomTextForPresenceAudience(uuid.UUID)
}

// KeyResetSessionDisconnectorForTest is the test-only destructive-reset
// session termination seam.
type KeyResetSessionDisconnectorForTest interface {
	DisconnectUser(uuid.UUID)
}

// SetCustomTextResetBroadcasterForTest installs a deterministic reset clearer.
func SetCustomTextResetBroadcasterForTest(h *Handler, broadcaster CustomTextResetBroadcasterForTest) {
	h.customTextResetBroadcaster = broadcaster
}

// SetKeyResetSessionDisconnectorForTest installs a deterministic session
// termination test double.
func SetKeyResetSessionDisconnectorForTest(h *Handler, disconnector KeyResetSessionDisconnectorForTest) {
	h.sessionDisconnector = disconnector
}
