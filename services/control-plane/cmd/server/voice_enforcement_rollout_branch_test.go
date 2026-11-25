package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the exact exit code, stdout and stderr of every
// runVoiceEnforcementRollout branch, so a refactor of its control flow
// cannot silently move a message, change a code or add output.

const rolloutUsageLine = "usage: control-plane voice-enforcement-rollout activate --confirm-drained --receipt-file <path>|deactivate|status [--json]|retire-boot <exact runtime evidence>|verify-retired-boot --node-boot-id UUID\n"

const unreachableRolloutDatabase = "postgres://x:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1" // #nosec G101 -- unreachable test DSN, not a real credential

type rolloutOutcome struct {
	code   int
	stdout string
	stderr string
}

func runRolloutOutcome(t *testing.T, args ...string) rolloutOutcome {
	t.Helper()
	code, stdout, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(args) })
	return rolloutOutcome{code: code, stdout: stdout, stderr: stderr}
}

func expectedRolloutStatusText(t *testing.T, db familyRolloutQuerier) string {
	t.Helper()
	status, err := readFamilyRolloutStatus(context.Background(), db)
	require.NoError(t, err)
	return fmt.Sprintf("voice-enforcement-rollout: %s\nvoice-enforcement-rollout: registryRows=%d\nvoice-enforcement-rollout: dmParentRows=%d\nvoice-enforcement-rollout: credentialParentRows=%d\nvoice-enforcement-rollout: dmReconciliationRows=%d\n",
		status.FamilyActivation, status.RegistryRows, status.DMParentRows, status.CredentialParentRows, status.DMReconciliationRows)
}

func expectedRolloutStatusJSON(t *testing.T, db familyRolloutQuerier) string {
	t.Helper()
	status, err := readFamilyRolloutStatus(context.Background(), db)
	require.NoError(t, err)
	raw, err := json.Marshal(status)
	require.NoError(t, err)
	return string(raw) + "\n"
}

func writeRolloutReceipt(t *testing.T, receipt map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	return receiptFile(t, raw)
}

func TestRunVoiceEnforcementRolloutBranchOutputsWithoutDatabase(t *testing.T) {
	const boot = "12abcdef-1234-4123-8123-123456789abc"
	missingReceipt := filepath.Join(t.TempDir(), "absent.json")
	cases := []struct {
		name  string
		env   map[string]string
		args  []string
		want  rolloutOutcome
		setup func(t *testing.T)
	}{
		{name: "usage", args: []string{"status", "extra"}, want: rolloutOutcome{64, "", rolloutUsageLine}},
		{name: "empty-usage", args: nil, want: rolloutOutcome{64, "", rolloutUsageLine}},
		{name: "config", env: map[string]string{"MFA_ENCRYPTION_KEY_VERSION": "not-an-integer"}, args: []string{"status"},
			want: rolloutOutcome{1, "", "load configuration failed\n"}},
		{name: "retire-boot-placement-mismatch", env: map[string]string{"ROOM_PLACEMENT_ENABLED": "false"},
			args: operatorRetirementArgs(boot, "media-plane-blue", "container"),
			want: rolloutOutcome{64, "", "usage: invalid boot retirement evidence\n"}},
		{name: "activate-missing-receipt", args: []string{"activate", "--confirm-drained", "--receipt-file", missingReceipt},
			want: rolloutOutcome{1, "", "invalid family activation receipt\n"}},
		{name: "status-open-database", args: []string{"status"}, want: rolloutOutcome{1, "", "open database failed\n"}},
		{name: "status-json-open-database", args: []string{"status", "--json"}, want: rolloutOutcome{1, "", "open database failed\n"}},
		{name: "deactivate-open-database", args: []string{"deactivate"}, want: rolloutOutcome{1, "", "open database failed\n"}},
		{name: "retire-open-database", env: map[string]string{"ROOM_PLACEMENT_ENABLED": "false"},
			args: operatorRetirementArgs(boot, "media-plane", "native"), want: rolloutOutcome{1, "", "open database failed\n"}},
		{name: "verify-open-database", args: []string{"verify-retired-boot", "--node-boot-id", boot},
			want: rolloutOutcome{1, "", "open database failed\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", unreachableRolloutDatabase)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			assert.Equal(t, tc.want, runRolloutOutcome(t, tc.args...))
		})
	}
}

func TestRunVoiceEnforcementRolloutBranchOutputsStatusAndMutation(t *testing.T) {
	t.Run("status-text", func(t *testing.T) {
		db := setupFamilyRolloutDB(t)
		want := expectedRolloutStatusText(t, db)
		assert.Contains(t, want, "voice-enforcement-rollout: prepared\n")
		assert.Equal(t, rolloutOutcome{0, want, ""}, runRolloutOutcome(t, "status"))
	})
	t.Run("status-json", func(t *testing.T) {
		db := setupFamilyRolloutDB(t)
		assert.Equal(t, rolloutOutcome{0, expectedRolloutStatusJSON(t, db), ""}, runRolloutOutcome(t, "status", "--json"))
	})
	t.Run("deactivate-prepared-prints-status", func(t *testing.T) {
		db := setupFamilyRolloutDB(t)
		assert.Equal(t, rolloutOutcome{0, expectedRolloutStatusText(t, db), ""}, runRolloutOutcome(t, "deactivate"))
	})
	t.Run("activate-then-repeat-then-refusals", func(t *testing.T) {
		db := setupFamilyRolloutDB(t)
		receipt := rolloutReceipt(t, db)
		path := writeRolloutReceipt(t, receipt)
		activated := runRolloutOutcome(t, "activate", "--confirm-drained", "--receipt-file", path)
		want := expectedRolloutStatusText(t, db)
		assert.Contains(t, want, "voice-enforcement-rollout: active\n")
		assert.Equal(t, rolloutOutcome{0, want, ""}, activated)
		// The active branch re-verifies the identical receipt and changes nothing.
		assert.Equal(t, rolloutOutcome{0, want, ""}, runRolloutOutcome(t, "activate", "--confirm-drained", "--receipt-file", path))
		receipt["activationId"] = uuid.New().String()
		assert.Equal(t, rolloutOutcome{1, "", "family rollout mutation refused\n"},
			runRolloutOutcome(t, "activate", "--confirm-drained", "--receipt-file", writeRolloutReceipt(t, receipt)))
		assert.Equal(t, rolloutOutcome{1, "", "family rollout mutation refused\n"}, runRolloutOutcome(t, "deactivate"))
		assert.Equal(t, rolloutOutcome{0, want, ""}, runRolloutOutcome(t, "status"))
	})
	t.Run("activate-count-mismatch-refused", func(t *testing.T) {
		db := setupFamilyRolloutDB(t)
		receipt := rolloutReceipt(t, db)
		_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, uuid.New())
		require.NoError(t, err)
		assert.Equal(t, rolloutOutcome{1, "", "family rollout mutation refused\n"},
			runRolloutOutcome(t, "activate", "--confirm-drained", "--receipt-file", writeRolloutReceipt(t, receipt)))
		assert.Contains(t, expectedRolloutStatusText(t, db), "voice-enforcement-rollout: prepared\n")
	})
}

func TestRunVoiceEnforcementRolloutBranchOutputsBootRetirement(t *testing.T) {
	t.Run("retire-absent-boot", func(t *testing.T) {
		setupFamilyRolloutDB(t)
		args := operatorRetirementArgs(uuid.New().String(), "media-plane", "container")
		assert.Equal(t, rolloutOutcome{0, "", ""}, runRolloutOutcome(t, args...))
	})
	t.Run("retire-live-boot-refused", func(t *testing.T) {
		db := setupFamilyRolloutDB(t)
		boot := uuid.New().String()
		_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
		require.NoError(t, err)
		addRetirementLiveConnection(t, db, boot)
		args := operatorRetirementArgs(boot, "media-plane", "native")
		assert.Equal(t, rolloutOutcome{1, "", "boot retirement refused\n"}, runRolloutOutcome(t, args...))
	})
	t.Run("verify-absent-boot", func(t *testing.T) {
		setupFamilyRolloutDB(t)
		assert.Equal(t, rolloutOutcome{3, "", ""}, runRolloutOutcome(t, "verify-retired-boot", "--node-boot-id", uuid.New().String()))
	})
	t.Run("verify-retired-boot", func(t *testing.T) {
		db := setupFamilyRolloutDB(t)
		boot := uuid.New().String()
		_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, boot)
		require.NoError(t, err)
		assert.Equal(t, rolloutOutcome{0, "", ""}, runRolloutOutcome(t, "verify-retired-boot", "--node-boot-id", boot))
	})
}
