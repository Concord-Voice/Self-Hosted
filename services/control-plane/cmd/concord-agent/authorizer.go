package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/hostagent"
)

const (
	authorizerPath      = "/etc/concord-voice/agent-authorizers/authorizers.json"
	maxAuthorizerImport = 1 << 20
	authorizerUsage     = `usage: concord-agent authorizer <command> [flags]
  list   prints each credential with its counter and revocation state
  import --file <export.json> --confirm-sha256 <sha256> [--reinstate]
         imports every credential in the export; each one both logs in and
         authorizes root actions, on one root counter
         --reinstate restores a credential that was revoked; without it an
         export holding a revoked credential is refused
  revoke --admin-id <uuid> [--allow-missing]
         an admin already revoked exits 0; one the store never held exits
         non-zero unless --allow-missing, which only an operator who has
         confirmed the ID should pass`
)

type authorizerStore interface {
	List() ([]hostagent.AuthorizerListing, error)
	ImportMany(hostagent.AuthorizerImport, bool) (int, error)
	Revoke(string) (int, error)
}

type authorizerStoreLoader interface {
	Load(string) (authorizerStore, error)
	LoadOrCreate(string, string, []string) (authorizerStore, error)
}

type fileAuthorizerStoreLoader struct{}

func (fileAuthorizerStoreLoader) Load(path string) (authorizerStore, error) {
	return hostagent.LoadAuthorizers(path)
}

func (fileAuthorizerStoreLoader) LoadOrCreate(path, rpID string, origins []string) (authorizerStore, error) {
	return hostagent.LoadOrCreateAuthorizers(path, rpID, origins)
}

// runAuthorizerCLI is deliberately separate from the socket server. Only a
// host-root operator may inspect or change the agent's independent trust root.
func runAuthorizerCLI(args []string) int {
	return runAuthorizerCLIAt(authorizerPath, args, os.Stdout, os.Stderr, os.Geteuid())
}

func runAuthorizerCLIAt(path string, args []string, stdout, stderr io.Writer, euid int) int {
	return runAuthorizerCLIWith(path, args, stdout, stderr, euid, fileAuthorizerStoreLoader{})
}

func runAuthorizerCLIWith(path string, args []string, stdout, stderr io.Writer, euid int, stores authorizerStoreLoader) int {
	if euid != 0 {
		_, _ = fmt.Fprintln(stderr, "authorizer: host root is required")
		return 1
	}
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, authorizerUsage)
		return 2
	}
	var err error
	switch args[0] {
	case "list":
		if len(args) != 1 {
			err = errors.New("list takes no arguments")
			break
		}
		var store authorizerStore
		store, err = stores.Load(path)
		if err != nil {
			break
		}
		var entries []hostagent.AuthorizerListing
		entries, err = store.List()
		if err == nil {
			err = json.NewEncoder(stdout).Encode(entries)
		}
	case "import":
		err = importAuthorizersWith(stores, path, args[1:], stdout, stderr)
	case "revoke":
		err = revokeAuthorizersWith(stores, path, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintln(stderr, authorizerUsage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "authorizer %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func importAuthorizersWith(stores authorizerStoreLoader, path string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "path to the admin export JSON")
	confirm := fs.String("confirm-sha256", "", "exact SHA-256 fingerprint of the export")
	reinstate := fs.Bool("reinstate", false, "restore credentials in the export that were revoked")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" || fs.NArg() != 0 {
		return errors.New("--file is required and no positional arguments are allowed")
	}
	f, err := os.Open(*file) //nolint:gosec // the root operator chooses the export file
	if err != nil {
		return fmt.Errorf("open export: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxAuthorizerImport+1 {
		return errors.New("export must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAuthorizerImport+2))
	if err != nil || len(data) > maxAuthorizerImport+1 {
		return errors.New("export exceeds size limit")
	}
	doc, fingerprint, err := parseAuthorizerImport(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "Authorizer export SHA-256: %s\n", fingerprint); err != nil {
		return err
	}
	if len(*confirm) != 64 || strings.ToLower(*confirm) != *confirm || !isLowerHex(*confirm) ||
		subtle.ConstantTimeCompare([]byte(*confirm), []byte(fingerprint)) != 1 {
		return errors.New("--confirm-sha256 must exactly match the export fingerprint")
	}
	store, err := stores.LoadOrCreate(path, doc.RPID, doc.Origins)
	if err != nil {
		return err
	}
	// A running agent that found no usable trust at startup restarts itself
	// within trustPollInterval of this import; say so whenever nothing was
	// active before.
	before, err := store.List()
	if err != nil {
		return err
	}
	firstImport := true
	for _, entry := range before {
		if !entry.Revoked {
			firstImport = false
			break
		}
	}
	added, err := store.ImportMany(doc, *reinstate)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Imported %d authorizer credentials for admin %s\n", added, doc.AdminID); err != nil {
		return err
	}
	if firstImport && added > 0 {
		_, err = fmt.Fprintln(stdout, "The running agent restarts itself within 15 seconds to enable newly imported authorizers. To enable them now: sudo systemctl restart concord-agent")
	}
	return err
}

func revokeAuthorizersWith(stores authorizerStoreLoader, path string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	adminID := fs.String("admin-id", "", "admin UUID to revoke")
	allowMissing := fs.Bool("allow-missing", false, "exit 0 when the admin has no entry in the store (operator has confirmed the ID)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *adminID == "" || fs.NArg() != 0 {
		return errors.New("--admin-id is required and no positional arguments are allowed")
	}
	store, err := stores.Load(path)
	if err != nil {
		return err
	}
	removed, err := store.Revoke(*adminID)
	if err != nil {
		return err
	}
	if removed == 0 {
		// A retry after a failed enrollment reset finds every credential already
		// revoked; that is success so the caller can go on to the reset.
		entries, lerr := store.List()
		if lerr != nil {
			return lerr
		}
		if slices.ContainsFunc(entries, func(e hostagent.AuthorizerListing) bool { return e.AdminID == *adminID }) {
			_, err = fmt.Fprintf(stderr, "Admin %s was already revoked; nothing more to revoke\n", *adminID)
			return err
		}
		// A wrong ID must not look revoked. Only a caller whose operator has
		// confirmed the account may accept an admin that was never imported.
		if !*allowMissing {
			return fmt.Errorf("no active authorizer credentials for admin %s; nothing was revoked", *adminID)
		}
		_, err = fmt.Fprintf(stderr, "Warning: admin %s has no Host Agent credentials; nothing to revoke\n", *adminID)
		return err
	}
	_, err = fmt.Fprintf(stdout, "Revoked %d authorizer credentials for admin %s\n", removed, *adminID)
	return err
}

// parseAuthorizerImport accepts only the exact JSON emitted by the admin CLI,
// followed by at most one newline. This makes the confirmed digest cover the
// document the store will validate, with no ignored keys or duplicate fields.
func parseAuthorizerImport(data []byte) (hostagent.AuthorizerImport, string, error) {
	var doc hostagent.AuthorizerImport
	if len(data) > maxAuthorizerImport+1 {
		return doc, "", errors.New("export exceeds size limit")
	}
	canonical := bytes.TrimSuffix(data, []byte{'\n'})
	if len(canonical) == 0 || len(canonical) > maxAuthorizerImport {
		return doc, "", errors.New("invalid export size")
	}
	if err := json.Unmarshal(canonical, &doc); err != nil {
		return doc, "", fmt.Errorf("decode export: %w", err)
	}
	encoded, err := json.Marshal(doc)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return doc, "", errors.New("export is not canonical authorizer JSON")
	}
	digest := sha256.Sum256(canonical)
	return doc, hex.EncodeToString(digest[:]), nil
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
