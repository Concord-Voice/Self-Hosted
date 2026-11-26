package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/hostagent"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestHostAgentProjectionEnabledByMountedDirectoryWithoutEnvMarker(t *testing.T) {
	t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "")
	dir := t.TempDir()
	require.True(t, hostAgentProjectionEnabled(dir))
	require.False(t, hostAgentProjectionEnabled(filepath.Join(dir, "missing")))

	t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "1")
	require.True(t, hostAgentProjectionEnabled(filepath.Join(dir, "missing")))
}

func TestAdminCtlLookupIDPrintsOnlyExistingAdminUUID(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := NewAdminRepo(db)
	username := "reset" + uuid.NewString()[:8]
	adminUser, err := repo.CreatePending(context.Background(), username, "hash")
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	deps := adminCtlDeps{repo: repo, stdout: &stdout, stderr: &stderr}
	require.Equal(t, 0, runAdminCtl(context.Background(), deps, []string{"lookup-id", "--username", username}))
	require.Equal(t, adminUser.ID+"\n", stdout.String())
	require.Empty(t, stderr.String())

	stdout.Reset()
	require.Equal(t, 1, runAdminCtl(context.Background(), deps, []string{"lookup-id", "--username", "missing"}))
	require.Empty(t, stdout.String(), "a failed lookup must not print a usable ID")
}

func TestResetEnrollmentRejectsImportedOrUnreadableAuthorizerBeforeDBMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		list func(string) ([]hostagent.AuthorizerListing, error)
	}{
		{"imported key", func(adminID string) ([]hostagent.AuthorizerListing, error) {
			return []hostagent.AuthorizerListing{{AdminID: adminID}}, nil
		}},
		{"unreadable projection", func(string) ([]hostagent.AuthorizerListing, error) {
			return nil, errors.New("unsafe authorizer file")
		}},
		{"missing projection directory", func(string) ([]hostagent.AuthorizerListing, error) {
			return nil, errors.New("unsafe authorizer directory")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "")
			projectionDir := t.TempDir()
			projectionPath := filepath.Join(projectionDir, "authorizers.json")
			db, cleanup := testdb.SetupTestDB(t)
			t.Cleanup(cleanup)
			rdb := redistest.Client(t)
			require.NoError(t, redistest.Reset(context.Background(), rdb))
			repo := NewAdminRepo(db)
			username := "reset" + uuid.NewString()[:8]
			adminUser, err := repo.CreatePending(context.Background(), username, "hash")
			require.NoError(t, err)
			require.NoError(t, repo.SetStatus(context.Background(), adminUser.ID, StatusActive))
			_, err = repo.AddCredential(context.Background(), AdminCredential{
				AdminID: adminUser.ID, CredentialID: []byte(uuid.NewString()), PublicKey: []byte{1}, AAGUID: make([]byte, 16),
			})
			require.NoError(t, err)
			var stdout bytes.Buffer
			deps := adminCtlDeps{
				repo: repo, audit: NewAuditLog(db), enroll: NewEnrollmentStore(rdb), stdout: &stdout, stderr: io.Discard,
				authorizerProjectionPath: projectionPath,
				listAuthorizers: func(path string) ([]hostagent.AuthorizerListing, error) {
					require.Equal(t, projectionPath, path)
					return tc.list(adminUser.ID)
				},
			}
			if !hostAgentProjectionEnabled(projectionDir) {
				deps.authorizerProjectionPath = ""
			}
			require.Equal(t, 1, runAdminCtl(context.Background(), deps, []string{"reset-enrollment", "--username", username}))
			require.Contains(t, stdout.String(), "authorizer")
			got, err := repo.GetByUsername(context.Background(), username)
			require.NoError(t, err)
			require.Equal(t, StatusActive, got.Status)
			count, err := repo.CountActiveCredentials(context.Background(), adminUser.ID)
			require.NoError(t, err)
			require.Equal(t, 1, count)
			require.NotContains(t, stdout.String(), "Token:")
		})
	}
}

func TestResetEnrollmentAllowsRevokedAuthorizerTombstone(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := NewAdminRepo(db)
	username := "reset" + uuid.NewString()[:8]
	adminUser, err := repo.CreatePending(context.Background(), username, "hash")
	require.NoError(t, err)
	rdb := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), rdb))
	var stdout bytes.Buffer
	deps := adminCtlDeps{
		repo: repo, audit: NewAuditLog(db), enroll: NewEnrollmentStore(rdb), stdout: &stdout, stderr: io.Discard,
		enrollBaseURL:            "https://admin.example.org",
		authorizerProjectionPath: "/etc/concord-voice/agent-authorizers/authorizers.json",
		listAuthorizers: func(string) ([]hostagent.AuthorizerListing, error) {
			return []hostagent.AuthorizerListing{{AdminID: adminUser.ID, Revoked: true}}, nil
		},
	}
	require.Equal(t, 0, runAdminCtl(context.Background(), deps, []string{"reset-enrollment", "--username", username}), stdout.String())
	require.Contains(t, stdout.String(), "Token:")
}

func TestResetEnrollmentAllowsAbsentAuthorizerFileInValidatedDirectory(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := NewAdminRepo(db)
	username := "reset" + uuid.NewString()[:8]
	adminUser, err := repo.CreatePending(context.Background(), username, "hash")
	require.NoError(t, err)
	rdb := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), rdb))
	var stdout bytes.Buffer
	deps := adminCtlDeps{
		repo: repo, audit: NewAuditLog(db), enroll: NewEnrollmentStore(rdb), stdout: &stdout, stderr: io.Discard,
		enrollBaseURL:            "https://admin.example.org",
		authorizerProjectionPath: "/etc/concord-voice/agent-authorizers/authorizers.json",
		listAuthorizers:          func(string) ([]hostagent.AuthorizerListing, error) { return nil, os.ErrNotExist },
	}
	require.Equal(t, 0, runAdminCtl(context.Background(), deps, []string{"reset-enrollment", "--username", username}), stdout.String())
	require.Contains(t, stdout.String(), "Token:")
	got, err := repo.GetByUsername(context.Background(), username)
	require.NoError(t, err)
	require.Equal(t, StatusPending, got.Status)
	require.Equal(t, adminUser.ID, got.ID)
}
