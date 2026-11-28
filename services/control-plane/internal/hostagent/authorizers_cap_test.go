package hostagent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// capTestCredentialID is a distinct credential ID of exactly size bytes.
func capTestCredentialID(n, size int) []byte {
	id := []byte(fmt.Sprintf("cred-%06d-", n))
	return append(id, []byte(strings.Repeat("x", size-len(id)))...)
}

func capTestImport(adminID, key string, from, count int) AuthorizerImport {
	in := AuthorizerImport{Schema: 1, RPID: "admin.example.test", Origins: []string{"https://admin.example.test"}, AdminID: adminID}
	for n := from; n < from+count; n++ {
		in.Credentials = append(in.Credentials, AuthorizerCredentialImport{
			CredentialID: base64.RawURLEncoding.EncodeToString(capTestCredentialID(n, 32)), PublicKey: key, SignCount: 1, Label: "key",
		})
	}
	return in
}

func capTestKey(t *testing.T) string {
	t.Helper()
	return testAuthorizerString(t, testAuthorizerEntry(t, testAuthorizerDoc(t)), "public_key")
}

func capTestListed(t *testing.T, store *AuthorizerStore, credentialID []byte) (AuthorizerListing, bool) {
	t.Helper()
	digest := sha256.Sum256(credentialID)
	listings, err := store.List()
	require.NoError(t, err)
	for _, listing := range listings {
		if listing.CredentialSHA256 == hex.EncodeToString(digest[:]) {
			return listing, true
		}
	}
	return AuthorizerListing{}, false
}

func TestAuthorizerRevokedRecordsDoNotHoldTheCap(t *testing.T) {
	root := t.TempDir()
	store, err := loadOrCreateAuthorizersAt(filepath.Join(root, "authorizers.json"), testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.NoError(t, err)
	key := capTestKey(t)
	first := uuid.NewString()
	added, err := store.ImportMany(capTestImport(first, key, 0, maxAuthorizers), false)
	require.NoError(t, err)
	require.Equal(t, maxAuthorizers, added)
	revoked, err := store.Revoke(first)
	require.NoError(t, err)
	require.Equal(t, maxAuthorizers, revoked)

	second := uuid.NewString()
	added, err = store.ImportMany(capTestImport(second, key, maxAuthorizers, 1), false)
	require.NoError(t, err, "64 revoked records must not use up the setup slots")
	require.Equal(t, 1, added)
	require.Len(t, testCredentials(t, store, second), 1)
	_, kept := capTestListed(t, store, capTestCredentialID(0, 32))
	require.True(t, kept, "a revoked record stays while the file has room")

	added, err = store.ImportMany(capTestImport(second, key, maxAuthorizers+1, maxAuthorizers-1), false)
	require.NoError(t, err)
	require.Equal(t, maxAuthorizers-1, added)
	_, err = store.ImportMany(capTestImport(uuid.NewString(), key, 2*maxAuthorizers, 1), false)
	require.Error(t, err, "the 65th active record must be refused")
	require.Len(t, testCredentials(t, store, second), maxAuthorizers)
}

func TestAuthorizerWriteRefusesAtSizeLimitAndKeepsRevoked(t *testing.T) {
	root := t.TempDir()
	key := capTestKey(t)
	revokedAdmin := uuid.NewString()
	record := func(n int, label string) authorizerRecord {
		return authorizerRecord{AdminID: revokedAdmin, CredentialID: base64.RawURLEncoding.EncodeToString(capTestCredentialID(n, maxAuthorizerIDBytes)), PublicKey: key, SignCount: 7, Label: label, Revoked: true}
	}
	// json.Marshal writes each '<' as six bytes, so these are near-maximum records.
	fillerLabel := strings.Repeat("<", maxAuthorizerLabel-8)
	doc := authorizerFile{Schema: 1, RPID: "admin.example.test", Origins: []string{"https://admin.example.test"}, Generation: 1, UpdatedAt: time.Now().UTC(), Authorizers: []authorizerRecord{}}
	empty, err := json.Marshal(doc)
	require.NoError(t, err)
	one, err := json.Marshal(record(0, fillerLabel))
	require.NoError(t, err)
	fillers := (maxAuthorizerFileBytes - len(empty)) / (len(one) + 1)
	for n := range fillers {
		doc.Authorizers = append(doc.Authorizers, record(n, fillerLabel))
	}
	path := writeAuthorizerDoc(t, root, doc)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(maxAuthorizerFileBytes-len(one)), "the file must start within one record of the limit")
	store := loadTestAuthorizers(t, path, root)

	in := capTestImport(uuid.NewString(), key, 0, 0)
	newID := capTestCredentialID(fillers, maxAuthorizerIDBytes)
	in.Credentials = []AuthorizerCredentialImport{{CredentialID: base64.RawURLEncoding.EncodeToString(newID), PublicKey: key, SignCount: 1, Label: strings.Repeat("<", maxAuthorizerLabel)}}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test reads its own t.TempDir file
	require.NoError(t, err)
	_, err = store.ImportMany(in, false)
	require.ErrorContains(t, err, "size limit reached", "a write past the size limit fails closed")

	after, err := os.ReadFile(path) //nolint:gosec // G304: test reads its own t.TempDir file
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused write leaves the file unchanged")
	_, ok := capTestListed(t, store, newID)
	require.False(t, ok)
	listing, ok := capTestListed(t, store, capTestCredentialID(0, maxAuthorizerIDBytes))
	require.True(t, ok, "revoked records are never dropped to make room")
	require.True(t, listing.Revoked)
	require.ErrorContains(t, store.AdvanceCounter(capTestCredentialID(0, maxAuthorizerIDBytes), 100), "revoked")
}
