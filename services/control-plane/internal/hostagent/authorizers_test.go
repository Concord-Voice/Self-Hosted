package hostagent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func testAuthorizerEntry(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	entries, ok := doc["authorizers"].([]map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, entries)
	return entries[0]
}

func testAuthorizerString(t *testing.T, entry map[string]any, key string) string {
	t.Helper()
	value, ok := entry[key].(string)
	require.True(t, ok, "authorizer %s must be a string", key)
	return value
}

func testAuthorizerDoc(t *testing.T) map[string]any {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	public, err := private.PublicKey.Bytes()
	require.NoError(t, err)
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	key, err := enc.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: public[1:33], -3: public[33:65]})
	require.NoError(t, err)
	return map[string]any{
		"schema": 1, "rp_id": "admin.example.test", "origins": []string{"https://admin.example.test"},
		"generation": 1, "updated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"authorizers": []map[string]any{{
			"admin_id": uuid.NewString(), "credential_id": base64.RawURLEncoding.EncodeToString([]byte("credential-1")),
			"public_key": base64.RawURLEncoding.EncodeToString(key), "sign_count": 4, "label": "primary hardware key",
		}},
	}
}

func writeAuthorizerDoc(t *testing.T, root string, doc any) string {
	t.Helper()
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(root, "authorizers.json")
	require.NoError(t, os.WriteFile(path, data, 0o644)) //nolint:gosec // root-owned public keys need the nonroot read-only projection
	return path
}

func loadTestAuthorizers(t *testing.T, path, root string) *AuthorizerStore {
	t.Helper()
	store, err := loadAuthorizersAt(path, testOwnerUID(), root)
	require.NoError(t, err)
	return store
}

func testOwnerUID() uint32 {
	return uint32(os.Geteuid()) //nolint:gosec // test process UIDs fit uint32 on supported hosts
}

func TestAuthorizerStoreValidAndCounterAdvance(t *testing.T) {
	root := t.TempDir()
	doc := testAuthorizerDoc(t)
	path := writeAuthorizerDoc(t, root, doc)
	store := loadTestAuthorizers(t, path, root)
	adminID := testAuthorizerString(t, testAuthorizerEntry(t, doc), "admin_id")
	creds := store.Credentials(adminID)
	require.Len(t, creds, 1)
	require.Equal(t, uint32(4), creds[0].Authenticator.SignCount)
	require.NoError(t, store.AdvanceCounter(creds[0].ID, 5))
	require.Equal(t, uint32(5), store.Credentials(adminID)[0].Authenticator.SignCount)
	require.Error(t, store.AdvanceCounter(creds[0].ID, 4))
	require.Empty(t, store.Credentials(uuid.NewString()))
}

func TestAuthorizerStoreRPConfigReturnsCloneAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	path := writeAuthorizerDoc(t, root, testAuthorizerDoc(t))
	store := loadTestAuthorizers(t, path, root)
	rpID, origins, err := store.RPConfig()
	require.NoError(t, err)
	require.Equal(t, "admin.example.test", rpID)
	origins[0] = "https://changed.example.test"
	_, current, err := store.RPConfig()
	require.NoError(t, err)
	require.Equal(t, []string{"https://admin.example.test"}, current)
	require.NoError(t, os.Chmod(path, 0o666)) //nolint:gosec // deliberate unsafe-mode fixture
	_, _, err = store.RPConfig()
	require.Error(t, err)
}

func TestAuthorizerStoreRejectsInvalidTrustFile(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"unknown schema":   func(d map[string]any) { d["schema"] = 2 },
		"invalid RP ID":    func(d map[string]any) { d["rp_id"] = ".." },
		"unrelated origin": func(d map[string]any) { d["origins"] = []string{"https://attacker.test"} },
		"unknown field":    func(d map[string]any) { d["surprise"] = true },
		"duplicate credential": func(d map[string]any) {
			entry := d["authorizers"].([]map[string]any)[0]
			d["authorizers"] = []map[string]any{entry, entry}
		},
		"malformed COSE": func(d map[string]any) {
			d["authorizers"].([]map[string]any)[0]["public_key"] = base64.RawURLEncoding.EncodeToString([]byte("bad"))
		},
		"noncanonical credential encoding": func(d map[string]any) {
			d["authorizers"].([]map[string]any)[0]["credential_id"] = "YQ=="
		},
		"oversized label": func(d map[string]any) {
			d["authorizers"].([]map[string]any)[0]["label"] = strings.Repeat("x", 129)
		},
		"too many credentials": func(d map[string]any) {
			entry := d["authorizers"].([]map[string]any)[0]
			entries := make([]map[string]any, 65)
			for i := range entries {
				clone := make(map[string]any, len(entry))
				for k, v := range entry {
					clone[k] = v
				}
				clone["credential_id"] = base64.RawURLEncoding.EncodeToString([]byte{byte(i + 1)})
				entries[i] = clone
			}
			d["authorizers"] = entries
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			doc := testAuthorizerDoc(t)
			mutate(doc)
			path := writeAuthorizerDoc(t, root, doc)
			_, err := loadAuthorizersAt(path, testOwnerUID(), root)
			require.Error(t, err)
		})
	}
}

func TestAuthorizerStoreRejectsOversizedFileAndTrailingJSON(t *testing.T) {
	root := t.TempDir()
	path := writeAuthorizerDoc(t, root, testAuthorizerDoc(t))
	content, err := os.ReadFile(path) //nolint:gosec // path is this test's temporary fixture
	require.NoError(t, err)
	for _, data := range [][]byte{
		append(append([]byte(nil), content...), []byte(` {"schema":1}`)...),
		append(append([]byte(nil), content...), []byte(strings.Repeat(" ", maxAuthorizerFileBytes))...),
	} {
		require.NoError(t, os.WriteFile(path, data, 0o644)) //nolint:gosec // path is this test's temporary fixture
		_, err := loadAuthorizersAt(path, testOwnerUID(), root)
		require.Error(t, err)
	}
}

func TestAuthorizerStoreRejectsUnsafePath(t *testing.T) {
	root := t.TempDir()
	path := writeAuthorizerDoc(t, root, testAuthorizerDoc(t))
	_, err := LoadAuthorizers(path)
	require.Error(t, err, "the public loader never trusts an unprivileged file")
	require.NoError(t, os.Chmod(path, 0o666)) //nolint:gosec // deliberately unsafe-mode fixture
	_, err = loadAuthorizersAt(path, testOwnerUID(), root)
	require.Error(t, err)
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // restore the required public-key file mode
	_, err = loadAuthorizersAt(path, testOwnerUID()+1, root)
	require.Error(t, err)

	link := filepath.Join(root, "link.json")
	require.NoError(t, os.Symlink(path, link))
	_, err = loadAuthorizersAt(link, testOwnerUID(), root)
	require.Error(t, err)
	child := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	childFile := writeAuthorizerDoc(t, child, testAuthorizerDoc(t))
	loadTestAuthorizers(t, childFile, root)
	linkDir := filepath.Join(root, "linkdir")
	require.NoError(t, os.Symlink(child, linkDir))
	_, err = loadAuthorizersAt(filepath.Join(linkDir, "authorizers.json"), testOwnerUID(), root)
	require.Error(t, err)
	require.NoError(t, os.Chmod(child, 0o777)) //nolint:gosec // deliberately unsafe-mode fixture
	_, err = loadAuthorizersAt(filepath.Join(child, "authorizers.json"), testOwnerUID(), root)
	require.Error(t, err)
}

func TestAuthorizerStoreAtomicWriteFailurePreservesFile(t *testing.T) {
	for _, fail := range []string{"fsync", "rename"} {
		t.Run(fail, func(t *testing.T) {
			root := t.TempDir()
			doc := testAuthorizerDoc(t)
			path := writeAuthorizerDoc(t, root, doc)
			store := loadTestAuthorizers(t, path, root)
			before, err := os.ReadFile(path) //nolint:gosec // path is this test's temporary fixture
			require.NoError(t, err)
			if fail == "fsync" {
				store.syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
			} else {
				store.renameFile = func(string, string) error { return errors.New("injected rename failure") }
			}
			adminID := testAuthorizerString(t, testAuthorizerEntry(t, doc), "admin_id")
			id := store.Credentials(adminID)[0].ID
			require.Error(t, store.AdvanceCounter(id, 5))
			after, err := os.ReadFile(path) //nolint:gosec // path is this test's temporary fixture
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func testAuthorizerImport(t *testing.T) AuthorizerImport {
	t.Helper()
	doc := testAuthorizerDoc(t)
	entry := testAuthorizerEntry(t, doc)
	return AuthorizerImport{
		Schema: 1, RPID: "admin.example.test", Origins: []string{"https://admin.example.test"},
		AdminID: testAuthorizerString(t, entry, "admin_id"),
		Credentials: []AuthorizerCredentialImport{{
			CredentialID: testAuthorizerString(t, entry, "credential_id"), PublicKey: testAuthorizerString(t, entry, "public_key"),
			SignCount: 4, Label: "primary hardware key",
		}},
	}
}

func TestAuthorizerStoreCreateImportRevoke(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "authorizers.json")
	store, err := loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "the nonroot read-only projection needs world-read, never world-write")
	require.Empty(t, store.Credentials(uuid.NewString()))

	doc := testAuthorizerImport(t)
	adminID := doc.AdminID
	second := doc.Credentials[0]
	second.CredentialID = base64.RawURLEncoding.EncodeToString([]byte("credential-2"))
	doc.Credentials = append(doc.Credentials, second)
	created, err := store.ImportMany(doc)
	require.NoError(t, err)
	require.Equal(t, 2, created)
	require.Len(t, store.Credentials(doc.AdminID), 2)
	id, err := base64.RawURLEncoding.DecodeString(doc.Credentials[0].CredentialID)
	require.NoError(t, err)
	require.NoError(t, store.AdvanceCounter(id, 5))
	created, err = store.ImportMany(doc)
	require.NoError(t, err)
	require.Zero(t, created)
	require.Equal(t, uint32(5), store.Credentials(doc.AdminID)[0].Authenticator.SignCount)
	doc.AdminID = uuid.NewString()
	created, err = store.ImportMany(doc)
	require.Error(t, err)
	require.Zero(t, created)
	require.Equal(t, uint32(5), store.Credentials(adminID)[0].Authenticator.SignCount)

	removed, err := store.Revoke(adminID)
	require.NoError(t, err)
	require.Equal(t, 2, removed)
	require.Empty(t, store.Credentials(adminID))
	removed, err = store.Revoke(adminID)
	require.NoError(t, err)
	require.Zero(t, removed)
}

func TestAuthorizerStoreImportBatchRejectsAllOnMalformedEntry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "authorizers.json")
	store, err := loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.NoError(t, err)
	doc := testAuthorizerImport(t)
	bad := doc.Credentials[0]
	bad.CredentialID = base64.RawURLEncoding.EncodeToString([]byte("different-credential"))
	bad.Label = strings.Repeat("x", maxAuthorizerLabel+1)
	doc.Credentials = append(doc.Credentials, bad)
	created, err := store.ImportMany(doc)
	require.Error(t, err)
	require.Zero(t, created)
	require.Empty(t, store.Credentials(doc.AdminID))

	doc.Credentials[1] = doc.Credentials[0]
	created, err = store.ImportMany(doc)
	require.Error(t, err)
	require.Zero(t, created)
	require.Empty(t, store.Credentials(doc.AdminID))
}

func TestAuthorizerStoreImportDoesNotReplaceTrustedCredential(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "authorizers.json")
	store, err := loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.NoError(t, err)
	imp := testAuthorizerImport(t)
	_, err = store.ImportMany(imp)
	require.NoError(t, err)
	id, err := base64.RawURLEncoding.DecodeString(imp.Credentials[0].CredentialID)
	require.NoError(t, err)
	require.NoError(t, store.AdvanceCounter(id, 8))

	stale := imp
	stale.Credentials = append([]AuthorizerCredentialImport(nil), imp.Credentials...)
	stale.Credentials[0].SignCount = 1
	stale.Credentials[0].Label = "stale label"
	created, err := store.ImportMany(stale)
	require.NoError(t, err)
	require.Zero(t, created)
	list, err := store.List()
	require.NoError(t, err)
	require.Equal(t, uint32(8), list[0].SignCount)
	require.Equal(t, "primary hardware key", list[0].Label)

	conflict := imp
	conflict.AdminID = uuid.NewString()
	before, err := os.ReadFile(path) //nolint:gosec // test-created trust file
	require.NoError(t, err)
	created, err = store.ImportMany(conflict)
	require.Error(t, err)
	require.Zero(t, created)
	after, err := os.ReadFile(path) //nolint:gosec // test-created trust file
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAuthorizerStoreListExposesOnlyBoundedMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "authorizers.json")
	store, err := loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.NoError(t, err)
	doc := testAuthorizerImport(t)
	_, err = store.ImportMany(doc)
	require.NoError(t, err)
	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	id, err := base64.RawURLEncoding.DecodeString(doc.Credentials[0].CredentialID)
	require.NoError(t, err)
	digest := sha256.Sum256(id)
	require.Equal(t, hex.EncodeToString(digest[:]), list[0].CredentialSHA256)
	require.Equal(t, doc.AdminID, list[0].AdminID)
	encoded, err := json.Marshal(list)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), doc.Credentials[0].CredentialID)
	require.NotContains(t, string(encoded), doc.Credentials[0].PublicKey)
	require.NoError(t, os.Chmod(path, 0o666)) //nolint:gosec // deliberate bad-mode fixture
	_, err = store.List()
	require.Error(t, err)
}

func TestAuthorizerStoreCreateRejectsConfigMismatchAndUnsafeExistingFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "authorizers.json")
	_, err := loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "..", []string{"https://admin.example.test"})
	require.Error(t, err)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.NoError(t, err)
	_, err = loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://wrong.example.test"})
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	unsafe := filepath.Join(root, "unsafe.json")
	require.NoError(t, os.WriteFile(unsafe, []byte(`{}`), 0o600))
	require.NoError(t, os.Symlink(unsafe, path))
	_, err = loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.Error(t, err)
}

func TestAuthorizerStoreImportFailurePreservesPriorFile(t *testing.T) {
	for _, fail := range []string{"fsync", "rename"} {
		t.Run(fail, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "authorizers.json")
			store, err := loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
			require.NoError(t, err)
			before, err := os.ReadFile(path) //nolint:gosec // path is this test's temporary fixture
			require.NoError(t, err)
			if fail == "fsync" {
				store.syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
			} else {
				store.renameFile = func(string, string) error { return errors.New("injected rename failure") }
			}
			created, err := store.ImportMany(testAuthorizerImport(t))
			require.Error(t, err)
			require.Zero(t, created)
			after, err := os.ReadFile(path) //nolint:gosec // path is this test's temporary fixture
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}
