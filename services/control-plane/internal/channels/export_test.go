package channels

import (
	"context"
	"time"
)

// SetResponseBudgetForTest overrides the expiration PATCH's response budget,
// so a test can show the handler hands its deadline to the work after commit.
func (h *Handler) SetResponseBudgetForTest(d time.Duration) { h.responseBudget = d }

// LegacyPendingKeyRequestIDsForTest returns the row IDs the pre-paging
// pending-keys reader returns for callerID, the oracle the paged reader's
// visibility is compared against.
func (h *Handler) LegacyPendingKeyRequestIDsForTest(ctx context.Context, callerID string) ([]string, error) {
	requests, err := h.legacyPendingKeyRequests(ctx, callerID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(requests))
	for _, req := range requests {
		ids = append(ids, req.ID)
	}
	return ids, nil
}
