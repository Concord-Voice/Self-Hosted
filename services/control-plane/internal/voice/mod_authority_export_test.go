package voice

import (
	"context"
	"database/sql"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/voicecommand"
	"github.com/gin-gonic/gin"
)

// ErrVoiceModPermissionForTest exposes the permission sentinel.
var ErrVoiceModPermissionForTest = errVoiceModPermission

// ErrVoiceModActorNotMemberForTest exposes the not-member sentinel.
var ErrVoiceModActorNotMemberForTest = errVoiceModActorNotMember

func FindVoiceChannelsTxForTest(ctx context.Context, tx *sql.Tx, serverID, targetID string) ([]string, error) {
	return findVoiceChannelsTx(ctx, tx, serverID, targetID)
}

func AuthorizeMoveDestinationTxForTest(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, destID string, selfMove bool,
) (rbac.Permission, error) {
	return authorizeMoveDestinationTx(ctx, tx, r, serverID, actorID, destID, selfMove)
}

func AuthorizeMoveSourcesTxForTest(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string, sources []string,
) ([]string, error) {
	return authorizeMoveSourcesTx(ctx, tx, r, serverID, actorID, sources)
}

func FilterAuthorizedSessionSpecsTxForTest(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string,
	perm rbac.Permission, specs []voicecommand.CommandSpec,
) ([]voicecommand.CommandSpec, error) {
	return filterAuthorizedSessionSpecsTx(ctx, tx, r, serverID, actorID, perm, specs)
}

func RequireActorVoiceChannelPermTxForTest(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, channelID string, perm rbac.Permission,
) error {
	return requireActorVoiceChannelPermTx(ctx, tx, r, serverID, actorID, channelID, perm)
}

// AuthorizeVoiceModWithForTest runs the preamble and reports (self, targetID, ok).
func AuthorizeVoiceModWithForTest(
	h *Handler, c *gin.Context, perm rbac.Permission, requireHierarchy, allowSelf bool,
) (self bool, targetID string, ok bool) {
	mod := h.authorizeVoiceModWith(c, voiceModPreamble{perm: perm, requireHierarchy: requireHierarchy, allowSelf: allowSelf})
	if mod == nil {
		return false, "", false
	}
	return mod.self, mod.targetID, true
}
