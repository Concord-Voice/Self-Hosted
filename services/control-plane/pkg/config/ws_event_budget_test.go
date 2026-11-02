package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWSEventBudgetParserDefaultsAndCanonicalValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   WSEventBudgetConfig
	}{
		{"absent", nil, WSEventBudgetConfig{Enabled: true, ScalePercent: 100}},
		{"enabled only", map[string]string{"WS_EVENT_BUDGET_ENABLED": "true"}, WSEventBudgetConfig{Enabled: true, ScalePercent: 100}},
		{"disabled only", map[string]string{"WS_EVENT_BUDGET_ENABLED": "false"}, WSEventBudgetConfig{Enabled: false, ScalePercent: 100}},
		{"minimum only", map[string]string{"WS_EVENT_BUDGET_SCALE_PERCENT": "25"}, WSEventBudgetConfig{Enabled: true, ScalePercent: 25}},
		{"explicit defaults", map[string]string{"WS_EVENT_BUDGET_ENABLED": "true", "WS_EVENT_BUDGET_SCALE_PERCENT": "100"}, WSEventBudgetConfig{Enabled: true, ScalePercent: 100}},
		{"disabled maximum", map[string]string{"WS_EVENT_BUDGET_ENABLED": "false", "WS_EVENT_BUDGET_SCALE_PERCENT": "400"}, WSEventBudgetConfig{Enabled: false, ScalePercent: 400}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadWSEventBudgetConfig(func(key string) (string, bool) { value, ok := tc.values[key]; return value, ok })
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWSEventBudgetParserRejectsNoncanonicalInput(t *testing.T) {
	for _, tc := range []struct {
		key    string
		values []string
	}{
		{"WS_EVENT_BUDGET_ENABLED", []string{"", "TRUE", "False", "1", "0", " true", "true ", "false\n", "synthetic-private-value"}},
		{"WS_EVENT_BUDGET_SCALE_PERCENT", []string{"", "0", "24", "401", "025", "0100", "+25", "-25", " 25", "25 ", "25\n", "２５", "٢٥", "25.0", "2.5e1", "18446744073709551616", strings.Repeat("7", 65536), "synthetic-private-value"}},
	} {
		for i, value := range tc.values {
			// Index-only names keep even malformed or oversized input out of test output.
			t.Run(tc.key+"/"+string(rune('a'+i)), func(t *testing.T) {
				_, err := loadWSEventBudgetConfig(func(key string) (string, bool) {
					if key == tc.key {
						return value, true
					}
					if key == "WS_EVENT_BUDGET_ENABLED" {
						return "false", true
					}
					return "", false
				})
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.key)
				if tc.key == "WS_EVENT_BUDGET_ENABLED" {
					assert.Contains(t, err.Error(), "true")
					assert.Contains(t, err.Error(), "false")
				} else {
					assert.Contains(t, err.Error(), "25")
					assert.Contains(t, err.Error(), "400")
				}
				if value == "synthetic-private-value" || len(value) > 1000 {
					assert.NotContains(t, err.Error(), value)
				}
			})
		}
	}
}

func TestWSEventBudgetLoadPropagatesPolicyAndParserErrors(t *testing.T) {
	// Match the package's existing development Load fixture; no network is used.
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ACTIVITY_HISTORY_CLUSTER_ENABLED", "false")
	t.Setenv("CONTROL_PLANE_REPLICA_COUNT", "1")
	t.Setenv("ROOM_PLACEMENT_ENABLED", "false")
	t.Setenv("REDIS_CREDENTIAL_MODE", "legacy")
	t.Setenv("WS_EVENT_BUDGET_ENABLED", "false")
	t.Setenv("WS_EVENT_BUDGET_SCALE_PERCENT", "237")
	cfg, err := Load()
	require.NoError(t, err, "positive control: otherwise valid Load must reach admission config")
	assert.Equal(t, WSEventBudgetConfig{Enabled: false, ScalePercent: 237}, cfg.WSEventBudget)
	t.Setenv("WS_EVENT_BUDGET_ENABLED", "true")
	t.Setenv("WS_EVENT_BUDGET_SCALE_PERCENT", "25")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, WSEventBudgetConfig{Enabled: true, ScalePercent: 25}, cfg.WSEventBudget)
	for _, key := range []string{"WS_EVENT_BUDGET_ENABLED", "WS_EVENT_BUDGET_SCALE_PERCENT"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "synthetic-private-value")
			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			assert.NotContains(t, err.Error(), "synthetic-private-value")
		})
	}
}
