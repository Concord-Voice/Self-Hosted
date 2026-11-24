package admin_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// The operator picks the action key from these lines, so each credential must
// be listed by its exported ID and label, followed by the purpose reminder.
func TestAdminCtl_ExportAgentAuthorizerListsCredentialsForPurposeChoice(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)
	username := uniqueAdminUsername("agent-export-lines")
	created, err := repo.CreatePending(ctx, username, "private-password-hash")
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
	require.NoError(t, repo.SetStatus(ctx, created.ID, admin.StatusActive))
	for _, c := range []struct{ id, label string }{{"b-login-key", "YubiKey desk"}, {"a-action-key", "root\x1b[31m key"}} {
		_, err := repo.AddCredential(ctx, admin.AdminCredential{
			AdminID: created.ID, CredentialID: []byte(c.id), PublicKey: []byte{1},
			AAGUID: make([]byte, 16), CredentialName: c.label, SignCount: 1,
		})
		require.NoError(t, err)
	}

	var stdout, stderr bytes.Buffer
	code := admin.RunAdminCtlWithStderrForTest(ctx, db, rdb, &stdout, &stderr, testEnrollBaseURL,
		[]string{"export-agent-authorizer", "--username", username})
	require.Equal(t, 0, code, "stderr: %s", stderr.String())

	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	require.Len(t, lines, 4, stderr.String())
	require.True(t, strings.HasPrefix(lines[0], "Authorizer export SHA-256: "), lines[0])
	enc := base64.RawURLEncoding.EncodeToString
	require.Equal(t, enc([]byte("a-action-key"))+"  root?[31m key", lines[1], "sorted like the export; control characters replaced")
	require.Equal(t, enc([]byte("b-login-key"))+"  YubiKey desk", lines[2])
	require.Equal(t, "import with --action-credential <id> to let a key authorize host writes; that key can then no longer be used to log in", lines[3])
	require.NotContains(t, stdout.String(), "--action-credential", "stdout stays the machine-readable bundle")
}
