package websocket

import (
	"errors"
	"time"
)

// EventClass is a closed admission category. Its zero value is invalid.
type EventClass uint8

// Closed event classes separate content, maintenance, and account operations.
const (
	EventClassContent EventClass = iota + 1
	EventClassTyping
	EventClassSubscription
	EventClassPresence
	EventClassProfile
	EventClassServer
	EventClassReady
	EventClassRenewal
)

// String returns only fixed class names, including for invalid values.
func (class EventClass) String() string {
	switch class {
	case EventClassContent:
		return "content"
	case EventClassTyping:
		return "typing"
	case EventClassSubscription:
		return "subscription"
	case EventClassPresence:
		return "presence"
	case EventClassProfile:
		return "profile"
	case EventClassServer:
		return "server"
	case EventClassReady:
		return "ready"
	case EventClassRenewal:
		return "renewal"
	default:
		return "unknown"
	}
}

// BudgetScope distinguishes existing, local, and account admission owners.
// Its zero value is invalid.
type BudgetScope uint8

// Budget scopes identify the gate that owns each event's allowance.
const (
	BudgetScopeExistingContent BudgetScope = iota + 1
	BudgetScopeLocalMaintenance
	BudgetScopeAccount
)

// EventPolicy is returned by value; changing it cannot change the registry.
// MayHold marks eligibility for a future bounded subscription/ready wait.
type EventPolicy struct {
	Class   EventClass
	Scope   BudgetScope
	MayHold bool
}

// EventBudgetConfig describes startup policy; it does not enable runtime gates.
// Environment parsing and absent-value defaults belong to the config owner.
type EventBudgetConfig struct {
	Enabled      bool
	ScalePercent int
}

type eventBudgetParameters struct {
	Interval time.Duration
	Burst    int
}

// ValidateEventBudgetConfig rejects invalid scales even when disabled.
func ValidateEventBudgetConfig(cfg EventBudgetConfig) error {
	if cfg.ScalePercent < 25 || cfg.ScalePercent > 400 {
		return errors.New("websocket event budget scale must be between 25 and 400")
	}
	return nil
}

// classifyInboundEvent covers application inputs, including intercepted renewal.
// Unknown names have no policy; protocol control and outbound frames are absent.
func classifyInboundEvent(event string) (EventPolicy, bool) {
	switch event {
	case "message", "dm_message":
		return EventPolicy{Class: EventClassContent, Scope: BudgetScopeExistingContent}, true
	case "typing", "dm_typing":
		return EventPolicy{Class: EventClassTyping, Scope: BudgetScopeAccount}, true
	case "subscribe", "unsubscribe", "subscribe_server", "unsubscribe_server", "subscribe_dm", "unsubscribe_dm":
		return EventPolicy{Class: EventClassSubscription, Scope: BudgetScopeAccount, MayHold: true}, true
	case "heartbeat":
		return EventPolicy{Class: EventClassPresence, Scope: BudgetScopeLocalMaintenance}, true
	case "set_status":
		return EventPolicy{Class: EventClassPresence, Scope: BudgetScopeAccount}, true
	case "profile_update":
		return EventPolicy{Class: EventClassProfile, Scope: BudgetScopeAccount}, true
	case "server_update":
		return EventPolicy{Class: EventClassServer, Scope: BudgetScopeAccount}, true
	case "connection_ready_probe":
		return EventPolicy{Class: EventClassReady, Scope: BudgetScopeAccount, MayHold: true}, true
	case "auth_renew":
		return EventPolicy{Class: EventClassRenewal, Scope: BudgetScopeAccount}, true
	default:
		return EventPolicy{}, false
	}
}

// accountEventBudget returns configured parameters independently of Enabled.
// Callers must use policy scope to exclude heartbeat from account admission.
func accountEventBudget(cfg EventBudgetConfig, class EventClass) (eventBudgetParameters, error) {
	if err := ValidateEventBudgetConfig(cfg); err != nil {
		return eventBudgetParameters{}, err
	}
	var intervalMicros int64
	var burst int
	switch class {
	case EventClassSubscription:
		intervalMicros, burst = 12500, 160
	case EventClassTyping:
		intervalMicros, burst = 100000, 40
	case EventClassProfile:
		intervalMicros, burst = 5000000, 8
	case EventClassServer, EventClassRenewal:
		intervalMicros, burst = 2000000, 16
	case EventClassPresence:
		intervalMicros, burst = 500000, 16
	case EventClassReady:
		intervalMicros, burst = 1000000, 16
	default:
		return eventBudgetParameters{}, errors.New("websocket event class has no account budget")
	}
	// Round intervals up in integer microseconds and bursts down, so rounding
	// never adds credit. Validated scale and fixed bases bound all arithmetic;
	// the smallest scaled burst is 2, so a minimum-one clamp is unnecessary.
	scale := int64(cfg.ScalePercent)
	intervalMicros = (intervalMicros*100 + scale - 1) / scale
	return eventBudgetParameters{
		Interval: time.Duration(intervalMicros) * time.Microsecond,
		Burst:    burst * cfg.ScalePercent / 100,
	}, nil
}
