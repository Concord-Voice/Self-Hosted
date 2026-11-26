//go:build integration

package managedredis_test

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/redistest"
)

// TestStepUpBudgetScriptsRunUnderManagedACL runs the step-up charge and busy
// refund scripts as a user holding exactly the managed control-plane grants.
// Redis checks every command a script calls against the caller's ACL, so a
// command the manifest leaves out (DECR, say) fails only here, as NOPERM
// inside the script, never in a check of top-level commands.
func TestStepUpBudgetScriptsRunUnderManagedACL(t *testing.T) {
	ctx := t.Context()
	admin := redistest.Client(t)
	const user = "cv_cp_stepup_acl_fixture"
	password := rand.Text()
	rule := strings.Fields(managedredis.EventBudgetTestControlPlaneACLRule())
	args := make([]any, 0, len(rule)+8)
	args = append(args, "ACL", "SETUSER", user, "reset", "on", ">"+password, "-@all")
	for _, token := range rule {
		args = append(args, token)
	}
	// SELECT only lets the fixture reach redistest's private database index;
	// production connects to database 0 and never needs it.
	args = append(args, "+select")
	require.NoError(t, admin.Do(ctx, args...).Err())
	t.Cleanup(func() { _ = admin.Do(ctx, "ACL", "DELUSER", user).Err() })

	opts := redistest.Options(t)
	opts.Username, opts.Password = user, password
	cp := redis.NewClient(opts)
	t.Cleanup(func() { _ = cp.Close() })

	for _, key := range []string{
		stepup.MFASettingsBudgetPrefix + "acl-fixture",
		"stepup:privacy_purge_fence:acl-fixture",
		"mfa_verify_attempts:acl-fixture",
	} {
		t.Run(key, func(t *testing.T) {
			t.Cleanup(func() { _ = admin.Del(ctx, key, key+":window").Err() })
			n, charged, err := stepup.ChargeKey(ctx, cp, key, time.Minute)
			require.NoError(t, err, "charge script under the managed ACL")
			require.Equal(t, int64(1), n)
			require.NoError(t, charged.Refund(ctx), "refund script under the managed ACL")
			require.Equal(t, "0", admin.Get(ctx, key).Val(), "the refund returned the attempt")
		})
	}
}
