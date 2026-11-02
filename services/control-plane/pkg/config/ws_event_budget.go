package config

import "errors"

// WSEventBudgetConfig is immutable process-start policy for WebSocket admission.
type WSEventBudgetConfig struct {
	Enabled      bool
	ScalePercent int
}

func loadWSEventBudgetConfig(lookup func(string) (string, bool)) (WSEventBudgetConfig, error) {
	cfg := WSEventBudgetConfig{Enabled: true, ScalePercent: 100}
	if raw, present := lookup("WS_EVENT_BUDGET_ENABLED"); present {
		switch raw {
		case "true":
			cfg.Enabled = true
		case "false":
			cfg.Enabled = false
		default:
			return WSEventBudgetConfig{}, errors.New("WS_EVENT_BUDGET_ENABLED must be true or false")
		}
	}
	if raw, present := lookup("WS_EVENT_BUDGET_SCALE_PERCENT"); present {
		scale, err := parseWSEventBudgetScale(raw)
		if err != nil {
			return WSEventBudgetConfig{}, err
		}
		cfg.ScalePercent = scale
	}
	return cfg, nil
}

func parseWSEventBudgetScale(raw string) (int, error) {
	invalid := errors.New("WS_EVENT_BUDGET_SCALE_PERCENT must be canonical unsigned ASCII decimal between 25 and 400")
	if len(raw) < 2 || len(raw) > 3 || raw[0] == '0' {
		return 0, invalid
	}
	scale := 0
	for i := range len(raw) {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, invalid
		}
		scale = scale*10 + int(raw[i]-'0')
	}
	if scale < 25 || scale > 400 {
		return 0, invalid
	}
	return scale, nil
}
