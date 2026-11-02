package websocket

// eventBudgetSnapshotProvider is optional: admission implementations need not
// expose scalar observations or change the EventAdmitter ownership contract.
type eventBudgetSnapshotProvider interface {
	eventBudgetSnapshot() (inflight int64, warmUsers int, capacityMisses uint64)
}

func (a *accountEventAdmitter) eventBudgetSnapshot() (inflight int64, warmUsers int, capacityMisses uint64) {
	if a == nil || !a.config.Enabled {
		return 0, 0, 0
	}

	inflight = a.activeRemote.Load()
	capacityMisses = a.capacityMisses.Load()
	a.warmMu.Lock()
	warmUsers = len(a.warm)
	a.warmMu.Unlock()
	return inflight, warmUsers, capacityMisses
}

// EventBudgetSnapshot observes the borrowed engine without owning its lifetime.
// Scalars are independently current, not a transaction. Close retains warm
// residents and cumulative misses; observation neither sweeps nor resets them.
func (h *Hub) EventBudgetSnapshot() (inflight int64, warmUsers int, capacityMisses uint64) {
	if h == nil || !h.eventBudgetConfig.Enabled {
		return 0, 0, 0
	}
	provider, ok := h.eventAdmitter.(eventBudgetSnapshotProvider)
	if !ok {
		return 0, 0, 0
	}
	return provider.eventBudgetSnapshot()
}
