package hostagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

const (
	maxAuthorizerFileBytes = 1 << 20
	maxAuthorizers         = 64
	maxAuthorizerIDBytes   = 1024
	maxAuthorizerKeyBytes  = 4096
	maxAuthorizerLabel     = 128
)

type authorizerRecord struct {
	AdminID      string `json:"admin_id"`
	CredentialID string `json:"credential_id"`
	PublicKey    string `json:"public_key"`
	SignCount    uint32 `json:"sign_count"`
	Label        string `json:"label"`
}

type authorizerFile struct {
	Schema      int                `json:"schema"`
	RPID        string             `json:"rp_id"`
	Origins     []string           `json:"origins"`
	Generation  uint64             `json:"generation"`
	UpdatedAt   time.Time          `json:"updated_at"`
	Authorizers []authorizerRecord `json:"authorizers"`
}

// AuthorizerCredentialImport is one public-key record in a root-confirmed
// export. It contains no secret material.
type AuthorizerCredentialImport struct {
	CredentialID string `json:"credential_id"`
	PublicKey    string `json:"public_key"`
	SignCount    uint32 `json:"sign_count"`
	Label        string `json:"label"`
}

// AuthorizerImport is the exact bounded document fingerprinted by the CLI.
type AuthorizerImport struct {
	Schema      int                          `json:"schema"`
	RPID        string                       `json:"rp_id"`
	Origins     []string                     `json:"origins"`
	AdminID     string                       `json:"admin_id"`
	Credentials []AuthorizerCredentialImport `json:"credentials"`
}

// AuthorizerListing reveals only metadata needed for root trust management.
type AuthorizerListing struct {
	AdminID          string `json:"admin_id"`
	Label            string `json:"label"`
	CredentialSHA256 string `json:"credential_sha256"`
	SignCount        uint32 `json:"sign_count"`
}

// AuthorizerStore reads root-owned public credentials. Every lookup re-reads the
// file so a root operator's revocation takes effect without restarting the agent.
type AuthorizerStore struct {
	path       string
	ownerUID   uint32
	trustRoot  string
	mu         sync.Mutex
	syncFile   func(*os.File) error
	renameFile func(string, string) error
}

// LoadAuthorizers accepts only a root-owned, mode-0644 regular file reached
// through root-owned, non-writable directory components.
func LoadAuthorizers(path string) (*AuthorizerStore, error) {
	return loadAuthorizersAt(path, 0, string(filepath.Separator))
}

// LoadOrCreateAuthorizers initializes an empty trust file for the first
// root-confirmed import, then requires exact RP configuration on later loads.
func LoadOrCreateAuthorizers(path, rpID string, origins []string) (*AuthorizerStore, error) {
	return loadOrCreateAuthorizersAt(path, 0, string(filepath.Separator), rpID, origins)
}

func loadOrCreateAuthorizersAt(path string, ownerUID uint32, trustRoot, rpID string, origins []string) (*AuthorizerStore, error) {
	s := &AuthorizerStore{path: filepath.Clean(path), ownerUID: ownerUID, trustRoot: filepath.Clean(trustRoot), syncFile: (*os.File).Sync, renameFile: os.Rename}
	initial := authorizerFile{Schema: 1, RPID: rpID, Origins: slices.Clone(origins), Generation: 1, UpdatedAt: time.Now().UTC(), Authorizers: []authorizerRecord{}}
	if err := validateAuthorizerFile(initial); err != nil {
		return nil, err
	}
	if err := s.checkPath(); err != nil {
		return nil, err
	}
	lock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	current, err := s.read()
	if errors.Is(err, os.ErrNotExist) {
		data, marshalErr := json.Marshal(initial)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if err := s.writeAtomic(data); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if current.RPID != rpID || !slices.Equal(current.Origins, origins) {
		return nil, errors.New("authorizer RP configuration mismatch")
	}
	return s, nil
}

// loadAuthorizersAt keeps the owner/path policy testable without weakening the
// public root-only entry point.
func loadAuthorizersAt(path string, ownerUID uint32, trustRoot string) (*AuthorizerStore, error) {
	s := &AuthorizerStore{path: filepath.Clean(path), ownerUID: ownerUID, trustRoot: filepath.Clean(trustRoot), syncFile: (*os.File).Sync, renameFile: os.Rename}
	if _, err := s.read(); err != nil {
		return nil, err
	}
	return s, nil
}

func ownedBy(info os.FileInfo, uid uint32) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
}

func (s *AuthorizerStore) checkPath() error {
	path, root := filepath.Clean(s.path), filepath.Clean(s.trustRoot)
	if !filepath.IsAbs(path) || !filepath.IsAbs(root) || !strings.HasPrefix(path, root+string(filepath.Separator)) && root != string(filepath.Separator) {
		return errors.New("authorizer path outside trust root")
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir) //nolint:gosec // checkPath confines each component to the absolute trust root and rejects symlinks
		if err != nil || !info.IsDir() || !ownedBy(info, s.ownerUID) || info.Mode().Perm()&0o022 != 0 {
			return errors.New("unsafe authorizer directory")
		}
		if dir == root {
			break
		}
		if dir == filepath.Dir(dir) {
			return errors.New("authorizer path outside trust root")
		}
	}
	return nil
}

func (s *AuthorizerStore) read() (authorizerFile, error) {
	if err := s.checkPath(); err != nil {
		return authorizerFile{}, err
	}
	fd, err := syscall.Open(s.path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return authorizerFile{}, fmt.Errorf("open authorizer file: %w", err)
	}
	f := os.NewFile(uintptr(fd), s.path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedBy(info, s.ownerUID) || info.Mode().Perm() != 0o644 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() > maxAuthorizerFileBytes {
		return authorizerFile{}, errors.New("unsafe authorizer file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAuthorizerFileBytes+1))
	if err != nil || len(data) > maxAuthorizerFileBytes || !utf8.Valid(data) {
		return authorizerFile{}, errors.New("invalid authorizer file")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc authorizerFile
	if err := dec.Decode(&doc); err != nil {
		return authorizerFile{}, fmt.Errorf("decode authorizer file: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return authorizerFile{}, errors.New("trailing authorizer data")
	}
	if err := validateAuthorizerFile(doc); err != nil {
		return authorizerFile{}, err
	}
	return doc, nil
}

func decodeAuthorizerField(encoded string, maxBytes int) ([]byte, error) {
	if encoded == "" || len(encoded) > base64.RawURLEncoding.EncodedLen(maxBytes) {
		return nil, errors.New("authorizer field size")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > maxBytes || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("authorizer field encoding")
	}
	return decoded, nil
}

func validateAuthorizerFile(doc authorizerFile) error {
	if doc.Schema != 1 || doc.Generation == 0 || doc.UpdatedAt.IsZero() || !validAuthorizerRPID(doc.RPID) || len(doc.Origins) == 0 || len(doc.Origins) > 8 || len(doc.Authorizers) > maxAuthorizers {
		return errors.New("invalid authorizer schema")
	}
	seenOrigins := make(map[string]bool, len(doc.Origins))
	for _, origin := range doc.Origins {
		if !validAuthorizerOrigin(origin, doc.RPID) || seenOrigins[origin] {
			return errors.New("invalid authorizer origin")
		}
		seenOrigins[origin] = true
	}
	seenIDs := make(map[string]bool, len(doc.Authorizers))
	for _, entry := range doc.Authorizers {
		if err := validateAuthorizerRecord(entry, seenIDs); err != nil {
			return err
		}
	}
	return nil
}

func validAuthorizerOrigin(origin, rpID string) bool {
	u, err := url.Parse(origin)
	return err == nil && len(origin) <= 512 && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.String() == origin && (u.Hostname() == rpID || strings.HasSuffix(u.Hostname(), "."+rpID))
}

func validAuthorizerLabel(label string) bool {
	return len(label) <= maxAuthorizerLabel && utf8.ValidString(label) && strings.IndexFunc(label, unicode.IsControl) < 0
}

func validateAuthorizerRecord(entry authorizerRecord, seenIDs map[string]bool) error {
	parsed, err := uuid.Parse(entry.AdminID)
	if err != nil || parsed.String() != entry.AdminID || !validAuthorizerLabel(entry.Label) {
		return errors.New("invalid authorizer entry")
	}
	id, err := decodeAuthorizerField(entry.CredentialID, maxAuthorizerIDBytes)
	if err != nil || seenIDs[string(id)] {
		return errors.New("duplicate or malformed authorizer credential")
	}
	key, err := decodeAuthorizerField(entry.PublicKey, maxAuthorizerKeyBytes)
	if err != nil {
		return errors.New("malformed authorizer key")
	}
	if _, err := webauthncose.ParsePublicKey(key); err != nil {
		return errors.New("malformed authorizer COSE key")
	}
	seenIDs[string(id)] = true
	return nil
}

func validAuthorizerRPID(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// Credentials returns a copy of the current trusted credentials for one admin.
// A malformed or revoked file fails closed to an empty result.
func (s *AuthorizerStore) Credentials(adminID string) []webauthn.Credential {
	doc, err := s.read()
	if err != nil {
		return nil
	}
	var credentials []webauthn.Credential
	for _, entry := range doc.Authorizers {
		if entry.AdminID != adminID {
			continue
		}
		id, _ := decodeAuthorizerField(entry.CredentialID, maxAuthorizerIDBytes)
		key, _ := decodeAuthorizerField(entry.PublicKey, maxAuthorizerKeyBytes)
		credentials = append(credentials, webauthn.Credential{ID: id, PublicKey: key, Authenticator: webauthn.Authenticator{SignCount: entry.SignCount}})
	}
	return credentials
}

// RPConfig returns the validated relying-party configuration for the agent's
// own WebAuthn verifier.
func (s *AuthorizerStore) RPConfig() (string, []string, error) {
	doc, err := s.read()
	if err != nil {
		return "", nil, err
	}
	return doc.RPID, slices.Clone(doc.Origins), nil
}

// List reads the validated current snapshot without exposing raw keys or IDs.
func (s *AuthorizerStore) List() ([]AuthorizerListing, error) {
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	list := make([]AuthorizerListing, 0, len(doc.Authorizers))
	for _, entry := range doc.Authorizers {
		id, _ := decodeAuthorizerField(entry.CredentialID, maxAuthorizerIDBytes)
		digest := sha256.Sum256(id)
		list = append(list, AuthorizerListing{AdminID: entry.AdminID, Label: entry.Label, CredentialSHA256: hex.EncodeToString(digest[:]), SignCount: entry.SignCount})
	}
	return list, nil
}

func (s *AuthorizerStore) lockFile() (*os.File, error) {
	if err := s.checkPath(); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(filepath.Dir(s.path), ".authorizers.lock")
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open authorizer lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedBy(info, s.ownerUID) || info.Mode().Perm() != 0o600 {
		_ = lock.Close()
		return nil, errors.New("unsafe authorizer lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock authorizer file: %w", err)
	}
	return lock, nil
}

func (s *AuthorizerStore) mutate(fn func(*authorizerFile) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	doc, err := s.read()
	if err != nil {
		return err
	}
	changed, err := fn(&doc)
	if err != nil || !changed {
		return err
	}
	if doc.Generation == ^uint64(0) {
		return errors.New("authorizer generation exhausted")
	}
	doc.Generation++
	doc.UpdatedAt = time.Now().UTC()
	if err := validateAuthorizerFile(doc); err != nil {
		return err
	}
	data, err := json.Marshal(doc)
	if err != nil || len(data) > maxAuthorizerFileBytes {
		return errors.New("encode authorizer file")
	}
	return s.writeAtomic(data)
}

// AdvanceCounter refuses stale counters and writes a new root-owned snapshot.
func (s *AuthorizerStore) AdvanceCounter(credentialID []byte, count uint32) error {
	return s.mutate(func(doc *authorizerFile) (bool, error) {
		found := false
		for i := range doc.Authorizers {
			id, _ := decodeAuthorizerField(doc.Authorizers[i].CredentialID, maxAuthorizerIDBytes)
			if !bytes.Equal(id, credentialID) {
				continue
			}
			old := doc.Authorizers[i].SignCount
			if count <= old && (count != 0 || old != 0) {
				return false, errors.New("authorizer counter did not advance")
			}
			if count == old {
				return false, nil
			}
			doc.Authorizers[i].SignCount = count
			found = true
			break
		}
		if !found {
			return false, errors.New("unknown authorizer credential")
		}
		return true, nil
	})
}

// ImportMany validates and installs one root-confirmed document atomically.
// A credential already trusted for the same admin/key is left unchanged, so a
// stale export cannot lower its durable sign counter.
func (s *AuthorizerStore) ImportMany(in AuthorizerImport) (int, error) {
	added := 0
	err := s.mutate(func(doc *authorizerFile) (bool, error) {
		if err := validateAuthorizerImport(in, *doc); err != nil {
			return false, err
		}
		seen := make(map[string]bool, len(in.Credentials))
		for _, input := range in.Credentials {
			candidate, err := importAuthorizerRecord(in.AdminID, input, seen)
			if err != nil {
				return false, err
			}
			created, err := addAuthorizerRecord(doc, candidate)
			if err != nil {
				return false, err
			}
			if created {
				added++
			}
		}
		if err := validateAuthorizerFile(*doc); err != nil {
			return false, err
		}
		return added > 0, nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

func validateAuthorizerImport(in AuthorizerImport, doc authorizerFile) error {
	if in.Schema != 1 || in.RPID != doc.RPID || !slices.Equal(in.Origins, doc.Origins) || len(in.Credentials) == 0 || len(in.Credentials) > maxAuthorizers {
		return errors.New("invalid authorizer import")
	}
	parsed, err := uuid.Parse(in.AdminID)
	if err != nil || parsed.String() != in.AdminID {
		return errors.New("invalid authorizer admin")
	}
	return nil
}

func importAuthorizerRecord(adminID string, input AuthorizerCredentialImport, seen map[string]bool) (authorizerRecord, error) {
	record := authorizerRecord{AdminID: adminID, CredentialID: input.CredentialID, PublicKey: input.PublicKey, SignCount: input.SignCount, Label: input.Label}
	if err := validateAuthorizerRecord(record, seen); err != nil {
		return authorizerRecord{}, err
	}
	return record, nil
}

func addAuthorizerRecord(doc *authorizerFile, candidate authorizerRecord) (bool, error) {
	for _, existing := range doc.Authorizers {
		if existing.CredentialID != candidate.CredentialID {
			continue
		}
		if existing.AdminID != candidate.AdminID || existing.PublicKey != candidate.PublicKey {
			return false, errors.New("conflicting import credential")
		}
		return false, nil
	}
	doc.Authorizers = append(doc.Authorizers, candidate)
	return true, nil
}

// Revoke removes every credential for one admin in one durable update.
func (s *AuthorizerStore) Revoke(adminID string) (int, error) {
	parsed, err := uuid.Parse(adminID)
	if err != nil || parsed.String() != adminID {
		return 0, errors.New("invalid authorizer admin")
	}
	removed := 0
	err = s.mutate(func(doc *authorizerFile) (bool, error) {
		kept := doc.Authorizers[:0]
		for _, entry := range doc.Authorizers {
			if entry.AdminID == adminID {
				removed++
			} else {
				kept = append(kept, entry)
			}
		}
		doc.Authorizers = kept
		return removed > 0, nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func (s *AuthorizerStore) writeAtomic(data []byte) error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".authorizers-")
	if err != nil {
		return fmt.Errorf("create authorizer temporary file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write authorizer temporary file: %w", err), tmp.Close())
	}
	if err := tmp.Chmod(0o644); err != nil {
		return errors.Join(fmt.Errorf("set authorizer file mode: %w", err), tmp.Close())
	}
	if err := s.syncFile(tmp); err != nil {
		return errors.Join(fmt.Errorf("sync authorizer temporary file: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close authorizer temporary file: %w", err)
	}
	if err := s.renameFile(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("replace authorizer file: %w", err)
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open authorizer directory: %w", err)
	}
	d := os.NewFile(uintptr(fd), dir)
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync authorizer directory: %w", err)
	}
	return nil
}
