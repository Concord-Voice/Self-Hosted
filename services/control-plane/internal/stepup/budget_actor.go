package stepup

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

// BudgetActor is an opaque snapshot for future authenticated step-up accounting.
// It grants no live authority or budget permit. Reservation must revalidate the
// full access identity and expected family through the canonical family guard.
type BudgetActor struct {
	identity         loginfamily.AccessIdentity
	expectedFamilyID uuid.UUID
}

// BudgetActorFromContext copies only AuthRequired's verified access provenance.
// Missing or inconsistent provenance is a wiring failure, not permission to
// reconstruct identity from raw claims or request-context values. The caller
// owns writing the returned error; construction has no response side effects.
func BudgetActorFromContext(c *gin.Context) (BudgetActor, *Error) {
	if c != nil {
		identity, grant, ok := middleware.VerifiedAccessFromContext(c)
		if ok {
			return BudgetActor{identity: identity, expectedFamilyID: grant.FamilyID}, nil
		}
	}
	return BudgetActor{}, &Error{
		Status: http.StatusInternalServerError,
		Body:   gin.H{"error": ErrMsgVerificationFailed},
	}
}
