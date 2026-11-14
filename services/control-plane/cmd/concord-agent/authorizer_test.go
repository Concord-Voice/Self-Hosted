package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/hostagent"
)

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
