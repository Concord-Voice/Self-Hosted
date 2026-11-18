package voice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/requestid"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	"github.com/gin-gonic/gin"
)

// Voice moderation authority (Concord-Voice/Security issue 168).
//
// A narrow-only gate: the server-scope bit stays necessary for every
// /servers/:id/voice/:userId/* action, and channel overrides can only refuse.
// The preamble authorizes the actor on the pool before any voice-state read,
// transaction or lock. Session actions and moves are additionally decided at
// the channel they touch, with the uncached transaction resolvers, inside the
// transaction that commits the effect. "Holds" always means resolver
// semantics: the server owner and server Administrators are not narrowed by
// channel overrides.

// voiceModPreamble selects the server-scope checks authorizeVoiceModWith runs.
type voiceModPreamble struct {
	perm             rbac.Permission
	requireHierarchy bool
	// allowSelf exempts an actor acting on themselves from the permission and
	// hierarchy steps. Only ServerMove sets it; membership is still required.
	allowSelf bool
}

// errVoiceModZeroPermission guards the channel helpers against a zero
// permission, which hasAllPerms would otherwise treat as held.
var errVoiceModZeroPermission = errors.New("voice moderation permission check called with no permission")

// authorizeVoiceModWith is the shared pool-side preamble. Order: path IDs (400)
// → membership (403 member) → server-scope permission (403 permission) →
// hierarchy (403). A self action on an allowSelf route skips the permission
// and hierarchy steps, so they are never called with a zero permission.
// Returns nil after writing the HTTP error response.
func (h *Handler) authorizeVoiceModWith(c *gin.Context, p voiceModPreamble) *voiceModContext {
	actorID := c.GetString("user_id")
	serverID, targetID, ok := canonicalVoiceModParams(c)
	if !ok {
		return nil
	}

	// Always verify actor is a member of the server
	if !h.checkMembership(c, serverID, actorID) {
		return nil
	}

	if p.allowSelf && sameVoiceModUser(actorID, targetID) {
		return &voiceModContext{serverID: serverID, targetID: targetID, self: true}
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, actorID, "", p.perm)
	if err != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", err, "permission", p.perm)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
		return nil
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return nil
	}

	if p.requireHierarchy && h.resolver.CheckHierarchy(c.Request.Context(), serverID, actorID, targetID) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
		return nil
	}

	return &voiceModContext{serverID: serverID, targetID: targetID}
}

// sameVoiceModUser compares an actor ID with an already-canonical target ID on
// canonical spellings. An actor ID that does not parse is never "self".
func sameVoiceModUser(actorID, canonicalTargetID string) bool {
	actor, err := requestid.Canonical(actorID)
	return err == nil && actor == canonicalTargetID
}

// hasAllPerms reports whether p holds every bit of required. Permission.Has is
// an any-of test and must not be used for a multi-bit requirement.
func hasAllPerms(p, required rbac.Permission) bool {
	return p&rbac.PermAdministrator != 0 || p&required == required
}

// classifyVoiceChannelResolveErr maps a channel-scoped resolve error. It tests
// rbac.ErrChannelNotInServer first: that error wraps rbac.ErrNotMember, and a
// missing or foreign channel must read as absent, never as "Not a member".
func classifyVoiceChannelResolveErr(err error) (present bool, out error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, rbac.ErrChannelNotInServer):
		return false, nil
	case errors.Is(err, rbac.ErrNotMember):
		return false, errVoiceModActorNotMember
	default:
		return false, fmt.Errorf("resolve actor voice channel permissions: %w", err)
	}
}

// resolveActorVoiceChannelTx resolves the actor's effective permissions on
// channelID inside tx. present is false, with a nil error, when the channel is
// not a channel of serverID or is not a voice channel; callers map absence.
func resolveActorVoiceChannelTx(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, channelID string,
) (perms rbac.Permission, present bool, err error) {
	channelID, err = requestid.Canonical(channelID)
	if err != nil {
		return 0, false, fmt.Errorf("resolve actor voice channel: invalid channel ID: %w", err)
	}
	perms, err = r.ResolveChannelPermissionsTx(ctx, tx, serverID, actorID, channelID)
	if present, err = classifyVoiceChannelResolveErr(err); !present || err != nil {
		return 0, false, err
	}
	isVoice, err := isVoiceChannelInServerTx(ctx, tx, channelID, serverID)
	if err != nil {
		return 0, false, fmt.Errorf("resolve actor voice channel type: %w", err)
	}
	if !isVoice {
		return 0, false, nil
	}
	return perms, true, nil
}

// findVoiceChannelsTx returns every voice channel of serverID the target has a
// participant row in, sorted so the first entry is deterministic. It takes no
// row lock: callers re-verify the source under the lifecycle lock.
func findVoiceChannelsTx(ctx context.Context, tx *sql.Tx, serverID, targetID string) (channelIDs []string, returnErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT vp.channel_id FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE c.server_id = $1 AND vp.user_id = $2
		ORDER BY vp.channel_id
	`, serverID, targetID)
	if err != nil {
		return nil, fmt.Errorf("find voice channels: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close voice channels: %w", closeErr))
		}
	}()
	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			return nil, fmt.Errorf("scan voice channel: %w", err)
		}
		channelIDs = append(channelIDs, channelID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate voice channels: %w", err)
	}
	return channelIDs, nil
}

// authorizeMoveDestinationTx decides the move destination for the actor: it
// must be a voice channel of this server on which the actor holds VIEW and
// JOIN, plus Move Members unless this is a self-move. Every refusal, including
// a missing, non-voice or foreign channel, is errVoiceModPermission. The
// returned permissions are the actor's on the destination.
func authorizeMoveDestinationTx(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, destID string, selfMove bool,
) (rbac.Permission, error) {
	required := rbac.PermViewVoiceChannels | rbac.PermJoinVoice
	if !selfMove {
		required |= rbac.PermMoveMembers
	}
	perms, present, err := resolveActorVoiceChannelTx(ctx, tx, r, serverID, actorID, destID)
	if err != nil {
		return 0, err
	}
	if !present || !hasAllPerms(perms, required) {
		return 0, errVoiceModPermission
	}
	return perms, nil
}

// authorizeMoveSourcesTx keeps the source channels the actor can see, in
// input order. A channel that is absent or that the actor cannot VIEW is
// dropped; a visible channel without Move Members refuses the whole move.
func authorizeMoveSourcesTx(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string, sources []string,
) (visible []string, err error) {
	for _, sourceID := range sources {
		perms, present, err := resolveActorVoiceChannelTx(ctx, tx, r, serverID, actorID, sourceID)
		if err != nil {
			return nil, err
		}
		if !present || !hasAllPerms(perms, rbac.PermViewVoiceChannels) {
			continue
		}
		if !hasAllPerms(perms, rbac.PermMoveMembers) {
			return nil, errVoiceModPermission
		}
		visible = append(visible, sourceID)
	}
	return visible, nil
}

// revalidateMoveActorTx repeats the actor's server-scope move decision inside
// the decision transaction: membership and Move Members, with no hierarchy
// step (ADR-0023). A self-move needs membership only.
func revalidateMoveActorTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, mod *voiceModContext, actorID string) error {
	if !mod.self {
		return revalidateVoiceModAuthorityTx(ctx, tx, r, mod, actorID, rbac.PermMoveMembers, false)
	}
	member, err := serverMemberExistsTx(ctx, tx, mod.serverID, actorID)
	if err != nil {
		return fmt.Errorf("revalidate self-move membership: %w", err)
	}
	if !member {
		return errVoiceModActorNotMember
	}
	return nil
}

// revalidateVoiceMoveAuthorityTx is the only authority closure a moderated
// move passes to the temporary grant. Inside the grant transaction it
// recomposes the whole move decision for a non-self move: membership and
// server-scope Move (no hierarchy), the destination stage, that the actor
// holds everything the grant confers (tempGrantAllow), and the source stage on
// sourceID. A source the actor can no longer see maps to
// errTemporaryGrantSourceChanged; every permission refusal, including a
// destination deleted since the decision, is errVoiceModPermission.
func revalidateVoiceMoveAuthorityTx(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, sourceID, destID string,
) error {
	if err := revalidateMoveActorTx(ctx, tx, r, &voiceModContext{serverID: serverID}, actorID); err != nil {
		return err
	}
	destPerms, err := authorizeMoveDestinationTx(ctx, tx, r, serverID, actorID, destID, false)
	if err != nil {
		return err
	}
	if !hasAllPerms(destPerms, tempGrantAllow) {
		return errVoiceModPermission
	}
	visible, err := authorizeMoveSourcesTx(ctx, tx, r, serverID, actorID, []string{sourceID})
	if err != nil {
		return err
	}
	if len(visible) == 0 {
		return errTemporaryGrantSourceChanged
	}
	return nil
}

// filterAuthorizedSessionSpecsTx keeps the session command specs whose channel
// the actor can see. For each distinct channel: absent or not VIEW drops its
// specs; VIEW without perm refuses the whole request, so an action is applied
// to every session the actor can see or refused. A spec without a channel ID
// is an internal error, never a pass.
func filterAuthorizedSessionSpecsTx(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string,
	perm rbac.Permission, specs []voicecommand.CommandSpec,
) ([]voicecommand.CommandSpec, error) {
	if perm == 0 {
		return nil, errVoiceModZeroPermission
	}
	for i, spec := range specs {
		if spec.ChannelID == nil || *spec.ChannelID == "" {
			return nil, fmt.Errorf("voice session command spec %d has no channel ID", i)
		}
	}
	visibleByChannel := make(map[string]bool, len(specs))
	kept := make([]voicecommand.CommandSpec, 0, len(specs))
	for _, spec := range specs {
		channelID := *spec.ChannelID
		visible, decided := visibleByChannel[channelID]
		if !decided {
			perms, present, err := resolveActorVoiceChannelTx(ctx, tx, r, serverID, actorID, channelID)
			if err != nil {
				return nil, err
			}
			visible = present && hasAllPerms(perms, rbac.PermViewVoiceChannels)
			if visible && !hasAllPerms(perms, perm) {
				return nil, errVoiceModPermission
			}
			visibleByChannel[channelID] = visible
		}
		if visible {
			kept = append(kept, spec)
		}
	}
	return kept, nil
}

// requireActorVoiceChannelPermTx requires channelID to be a voice channel of
// this server on which the actor holds VIEW and perm. Every refusal is
// errVoiceModPermission.
func requireActorVoiceChannelPermTx(
	ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, channelID string, perm rbac.Permission,
) error {
	if perm == 0 {
		return errVoiceModZeroPermission
	}
	perms, present, err := resolveActorVoiceChannelTx(ctx, tx, r, serverID, actorID, channelID)
	if err != nil {
		return err
	}
	if !present || !hasAllPerms(perms, rbac.PermViewVoiceChannels|perm) {
		return errVoiceModPermission
	}
	return nil
}
