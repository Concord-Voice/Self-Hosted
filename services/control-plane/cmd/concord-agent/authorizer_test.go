package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/hostagent"
)

type fakeAuthorizerStore struct {
	entries   []hostagent.AuthorizerListing
	imported  hostagent.AuthorizerImport
	reinstate bool
	listErr   error
	importErr error
	revokeErr error
}

func (s *fakeAuthorizerStore) List() ([]hostagent.AuthorizerListing, error) {
	return s.entries, s.listErr
}

func (s *fakeAuthorizerStore) ImportMany(doc hostagent.AuthorizerImport, reinstate bool) (int, error) {
	s.imported, s.reinstate = doc, reinstate
	if s.importErr == nil && !slices.ContainsFunc(s.entries, func(e hostagent.AuthorizerListing) bool { return e.AdminID == doc.AdminID && !e.Revoked }) {
		for _, c := range doc.Credentials {
			s.entries = append(s.entries, hostagent.AuthorizerListing{AdminID: doc.AdminID, Label: c.Label, SignCount: c.SignCount, CredentialSHA256: "hash"})
		}
	}
	return len(doc.Credentials), s.importErr
}

func (s *fakeAuthorizerStore) Revoke(adminID string) (int, error) {
	if s.revokeErr != nil {
		return 0, s.revokeErr
	}
	for i, entry := range s.entries {
		if entry.AdminID == adminID && !entry.Revoked {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return 1, nil
		}
	}
	return 0, nil
}

type fakeAuthorizerStoreLoader struct {
	store     *fakeAuthorizerStore
	loadErr   error
	createErr error
}

func (l fakeAuthorizerStoreLoader) Load(string) (authorizerStore, error) {
	return l.store, l.loadErr
}

func (l fakeAuthorizerStoreLoader) LoadOrCreate(string, string, []string) (authorizerStore, error) {
	return l.store, l.createErr
}

func TestParseAuthorizerImportExactCanonicalDocument(t *testing.T) {
	want := hostagent.AuthorizerImport{
		Schema: 1, RPID: "admin.example.org", Origins: []string{"https://admin.example.org"},
		AdminID:     "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6",
		Credentials: []hostagent.AuthorizerCredentialImport{{CredentialID: "AQID", PublicKey: "BAUG", SignCount: 2, Label: "key"}},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, fingerprint, err := parseAuthorizerImport(append(bytes.Clone(data), '\n'))
	if err != nil || got.AdminID != want.AdminID || len(got.Credentials) != 1 {
		t.Fatalf("parsed document %+v, fingerprint %q: %v", got, fingerprint, err)
	}
	digest := sha256.Sum256(data)
	if fingerprint != hex.EncodeToString(digest[:]) {
		t.Fatalf("fingerprint %q does not cover exact canonical bytes", fingerprint)
	}
	for name, changed := range map[string][]byte{
		"leading whitespace":  append([]byte(" "), data...),
		"trailing whitespace": append(bytes.Clone(data), ' '),
		"two newlines":        append(bytes.Clone(data), '\n', '\n'),
		"unknown field":       bytes.Replace(data, []byte(`"schema":1`), []byte(`"schema":1,"ignored":true`), 1),
		"duplicate field":     bytes.Replace(data, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1),
		"oversize":            bytes.Repeat([]byte("x"), maxAuthorizerImport+2),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseAuthorizerImport(changed); err == nil {
				t.Fatal("accepted noncanonical export")
			}
		})
	}
}

func TestAuthorizerCLIRequiresHostRootBeforeOpeningStore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAuthorizerCLIAt("/does/not/exist", []string{"list"}, &stdout, &stderr, 1000); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "host root is required") {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestFileAuthorizerStoreLoaderFailsClosed(t *testing.T) {
	loader := fileAuthorizerStoreLoader{}
	if _, err := loader.Load("/does/not/exist"); err == nil {
		t.Fatal("missing authorizer store loaded")
	}
	if _, err := loader.LoadOrCreate("/does/not/exist", "", nil); err == nil {
		t.Fatal("invalid authorizer configuration initialized a store")
	}
}

func TestAuthorizerImportRejectsWrongConfirmationWithoutStoreMutation(t *testing.T) {
	doc := hostagent.AuthorizerImport{Schema: 1, RPID: "admin.example.org", Origins: []string{"https://admin.example.org"}, AdminID: "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6", Credentials: []hostagent.AuthorizerCredentialImport{{CredentialID: "AQID", PublicKey: "BAUG", Label: "key"}}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	file := t.TempDir() + "/export.json"
	if err := os.WriteFile(file, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir() + "/authorizers.json"
	var stdout, stderr bytes.Buffer
	code := runAuthorizerCLIAt(store, []string{"import", "--file", file, "--confirm-sha256", strings.Repeat("0", 64)}, &stdout, &stderr, 0)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "must exactly match") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "AQID") || strings.Contains(stderr.String(), "BAUG") {
		t.Fatal("credential material appeared in CLI diagnostics")
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatalf("store changed before confirmation: %v", err)
	}
}

func TestAuthorizerCLIImportsListsAndRevokesRootConfirmedCredential(t *testing.T) {
	doc := hostagent.AuthorizerImport{
		Schema: 1, RPID: "admin.example.org", Origins: []string{"https://admin.example.org"},
		AdminID:     "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6",
		Credentials: []hostagent.AuthorizerCredentialImport{{CredentialID: "AQID", PublicKey: "BAUG", SignCount: 2, Label: "primary key"}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	export := t.TempDir() + "/export.json"
	if err := os.WriteFile(export, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	store := &fakeAuthorizerStore{}
	loader := fakeAuthorizerStoreLoader{store: store}
	storePath := t.TempDir() + "/authorizers.json"

	var stdout, stderr bytes.Buffer
	if code := runAuthorizerCLIWith(storePath, []string{"import", "--file", export, "--confirm-sha256", hex.EncodeToString(digest[:])}, &stdout, &stderr, 0, loader); code != 0 {
		t.Fatalf("import exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Imported 1 authorizer credentials") ||
		!strings.Contains(stdout.String(), "sudo systemctl restart concord-agent") ||
		!strings.Contains(stderr.String(), "Authorizer export SHA-256") {
		t.Fatalf("import stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if store.imported.AdminID != doc.AdminID {
		t.Fatalf("imported document %+v", store.imported)
	}
	if err := os.WriteFile(storePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAuthorizerCLIWith(storePath, []string{"import", "--file", export, "--confirm-sha256", hex.EncodeToString(digest[:])}, &stdout, &stderr, 0, loader); code != 0 {
		t.Fatalf("repeat import exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "restart concord-agent") {
		t.Fatalf("existing store requested a restart: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runAuthorizerCLIWith(storePath, []string{"list"}, &stdout, &stderr, 0, loader); code != 0 {
		t.Fatalf("list exit %d: %s", code, stderr.String())
	}
	var entries []hostagent.AuthorizerListing
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].AdminID != doc.AdminID || entries[0].Label != "primary key" || entries[0].SignCount != 2 || entries[0].CredentialSHA256 == "" {
		t.Fatalf("unexpected listing: %+v", entries)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runAuthorizerCLIWith(storePath, []string{"revoke", "--admin-id", doc.AdminID}, &stdout, &stderr, 0, loader); code != 0 {
		t.Fatalf("revoke exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked 1 authorizer credentials") {
		t.Fatalf("revoke stdout %q", stdout.String())
	}
}

func TestAuthorizerCLIReportsStoreErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"list":   {"list"},
		"revoke": {"revoke", "--admin-id", "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			loader := fakeAuthorizerStoreLoader{store: &fakeAuthorizerStore{listErr: errors.New("load failed")}, loadErr: errors.New("load failed")}
			if code := runAuthorizerCLIWith("/authorizers.json", args, &stdout, &stderr, 0, loader); code != 1 || !strings.Contains(stderr.String(), "load failed") {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestAuthorizerCLIRejectsMalformedCommands(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown":        {"unknown"},
		"list arguments": {"list", "extra"},
		"import missing": {"import"},
		"revoke missing": {"revoke"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runAuthorizerCLIAt("/does/not/exist", args, &stdout, &stderr, 0)
			if code == 0 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestIsLowerHex(t *testing.T) {
	for value, want := range map[string]bool{"0123abcdef": true, "": true, "ABCDEF": false, "abc-": false} {
		if got := isLowerHex(value); got != want {
			t.Errorf("isLowerHex(%q) = %t, want %t", value, got, want)
		}
	}
}

func writeTestExport(t *testing.T) (string, string, hostagent.AuthorizerImport) {
	t.Helper()
	doc := hostagent.AuthorizerImport{
		Schema: 1, RPID: "admin.example.org", Origins: []string{"https://admin.example.org"},
		AdminID:     "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6",
		Credentials: []hostagent.AuthorizerCredentialImport{{CredentialID: "AQID", PublicKey: "BAUG", SignCount: 2, Label: "primary key"}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	export := t.TempDir() + "/export.json"
	if err := os.WriteFile(export, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return export, hex.EncodeToString(digest[:]), doc
}

func TestAuthorizerImportHintsRestartWhenNoCredentialWasActive(t *testing.T) {
	export, digest, doc := writeTestExport(t)
	// The trust file exists, but every credential in it is revoked: a running
	// agent has nothing active, so the operator must be told to restart.
	store := &fakeAuthorizerStore{entries: []hostagent.AuthorizerListing{{AdminID: "11111111-2222-4333-8444-555555555555", Revoked: true}}}
	storePath := t.TempDir() + "/authorizers.json"
	if err := os.WriteFile(storePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runAuthorizerCLIWith(storePath, []string{"import", "--file", export, "--confirm-sha256", digest}, &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: store}); code != 0 {
		t.Fatalf("import exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sudo systemctl restart concord-agent") {
		t.Fatalf("no restart hint with zero active credentials: %q", stdout.String())
	}
	if store.imported.AdminID != doc.AdminID {
		t.Fatalf("imported %+v", store.imported)
	}
}

func TestAuthorizerImportPassesReinstateOnlyWhenAsked(t *testing.T) {
	export, digest, _ := writeTestExport(t)
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"import", "--file", export, "--confirm-sha256", digest}, false},
		{[]string{"import", "--file", export, "--confirm-sha256", digest, "--reinstate"}, true},
	} {
		store := &fakeAuthorizerStore{}
		var stdout, stderr bytes.Buffer
		if code := runAuthorizerCLIWith(t.TempDir()+"/a.json", tc.args, &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: store}); code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, stderr.String())
		}
		if store.reinstate != tc.want {
			t.Fatalf("%v: reinstate=%t, want %t", tc.args, store.reinstate, tc.want)
		}
	}
	store := &fakeAuthorizerStore{importErr: hostagent.ErrAuthorizerRevokedImport}
	var stdout, stderr bytes.Buffer
	if code := runAuthorizerCLIWith(t.TempDir()+"/a.json", []string{"import", "--file", export, "--confirm-sha256", digest}, &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: store}); code != 1 || !strings.Contains(stderr.String(), "--reinstate") {
		t.Fatalf("revoked import: exit %d stderr %q", code, stderr.String())
	}
	if !strings.Contains(authorizerUsage, "--reinstate") {
		t.Fatal("usage does not document --reinstate")
	}
}

func TestAuthorizerRevokeOfNeverImportedAdminFailsWithoutAllowMissing(t *testing.T) {
	store := &fakeAuthorizerStore{}
	var stdout, stderr bytes.Buffer
	code := runAuthorizerCLIWith("/authorizers.json", []string{"revoke", "--admin-id", "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6"}, &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: store})
	if code != 1 || !strings.Contains(stderr.String(), "nothing was revoked") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestAuthorizerRevokeOfNeverImportedAdminWarnsAndSucceeds(t *testing.T) {
	store := &fakeAuthorizerStore{}
	var stdout, stderr bytes.Buffer
	code := runAuthorizerCLIWith("/authorizers.json", []string{"revoke", "--allow-missing", "--admin-id", "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6"}, &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: store})
	if code != 0 || !strings.Contains(stderr.String(), "nothing to revoke") || strings.Contains(stdout.String(), "Revoked") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestAuthorizerRevokeOfAlreadyRevokedAdminSucceeds(t *testing.T) {
	const id = "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6"
	store := &fakeAuthorizerStore{entries: []hostagent.AuthorizerListing{{AdminID: id, Revoked: true}}}
	var stdout, stderr bytes.Buffer
	code := runAuthorizerCLIWith("/authorizers.json", []string{"revoke", "--admin-id", id}, &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: store})
	if code != 0 || !strings.Contains(stderr.String(), "already revoked") || strings.Contains(stdout.String(), "Revoked ") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestAuthorizerImportTrustsEveryExportedCredential(t *testing.T) {
	export, digest, doc := writeTestExport(t)
	base := []string{"import", "--file", export, "--confirm-sha256", digest}
	store := &fakeAuthorizerStore{}
	var stdout, stderr bytes.Buffer
	if code := runAuthorizerCLIWith(t.TempDir()+"/a.json", base, &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: store}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if len(store.imported.Credentials) != len(doc.Credentials) {
		t.Fatalf("imported %d credentials, want all %d", len(store.imported.Credentials), len(doc.Credentials))
	}
	if strings.Contains(stderr.String(), "Warning") || strings.Contains(stdout.String(), "purpose") {
		t.Fatalf("one-key import must not mention purposes: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	stderr.Reset()
	if code := runAuthorizerCLIWith(t.TempDir()+"/a.json", append(slices.Clone(base), "--action-credential", "AQID"), &stdout, &stderr, 0, fakeAuthorizerStoreLoader{store: &fakeAuthorizerStore{}}); code != 1 {
		t.Fatalf("the removed --action-credential flag must be refused, exit %d", code)
	}
	if strings.Contains(authorizerUsage, "--action-credential") || strings.Contains(authorizerUsage, "purpose") {
		t.Fatal("usage still documents the removed purpose split")
	}
}
