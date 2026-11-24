// Command credential-authority-vectors creates the test-only ADR-0051 v2 corpus.
package main

import (
	"bytes"
	"crypto/elliptic"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	credential "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/authoritycredential"
)

const (
	corpusPath    = "docs/design/credential-authority-v2-vectors.json"
	generatorPath = "services/control-plane/cmd/credential-authority-vectors/gen.go"
)

type vector struct {
	Name      string         `json:"name"`
	Suite     string         `json:"suite"`
	Operation string         `json:"operation"`
	Input     map[string]any `json:"input"`
	Expect    map[string]any `json:"expect"`
}

type corpus struct {
	Schema          string   `json:"schema"`
	Version         int      `json:"version"`
	GeneratorSHA384 string   `json:"generator_sha384"`
	VectorsSHA384   string   `json:"vectors_sha384"`
	Vectors         []vector `json:"vectors"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 2 {
		return errors.New("usage: credential-authority-vectors [--check|--write]")
	}
	mode := "--write"
	if len(os.Args) == 2 {
		mode = os.Args[1]
	}
	if mode != "--check" && mode != "--write" {
		return errors.New("usage: credential-authority-vectors [--check|--write]")
	}
	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	generated, err := generate(root)
	if err != nil {
		return err
	}
	path := filepath.Join(root, corpusPath)
	if mode == "--check" {
		current, err := os.ReadFile(path) // #nosec G304 -- path is fixed beneath the discovered repository root.
		if err != nil {
			return fmt.Errorf("read corpus: %w", err)
		}
		if !bytes.Equal(current, generated) {
			return errors.New("generated corpus differs from committed corpus")
		}
		return nil
	}
	if err := os.WriteFile(path, generated, 0o600); err != nil {
		return fmt.Errorf("write corpus: %w", err)
	}
	return nil
}

func repositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "CHANGELOG.md")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("repository root not found")
		}
		directory = parent
	}
}

func generate(root string) ([]byte, error) {
	source, err := os.ReadFile(filepath.Join(root, generatorPath)) // #nosec G304 -- generatorPath is a repository constant.
	if err != nil {
		return nil, fmt.Errorf("read generator: %w", err)
	}
	vectors, err := buildVectors()
	if err != nil {
		return nil, err
	}
	vectorsBytes, err := json.Marshal(vectors)
	if err != nil {
		return nil, fmt.Errorf("marshal vectors: %w", err)
	}
	generatorHash := sha384Hex(source)
	result := corpus{
		Schema:          "concord-credential-authority-v2-vectors",
		Version:         1,
		GeneratorSHA384: generatorHash,
		VectorsSHA384:   sha384Hex(vectorsBytes),
		Vectors:         vectors,
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal corpus: %w", err)
	}
	return append(encoded, '\n'), nil
}

func buildVectors() ([]vector, error) {
	publication := fixturePublication()
	publicationRaw, err := publication.Encode()
	if err != nil {
		return nil, err
	}
	publicationDigest, err := credential.PublicationDigest(publication)
	if err != nil {
		return nil, err
	}
	evidence := fixtureEvidence(publicationDigest, publication)
	evidenceRaw, err := evidence.Encode()
	if err != nil {
		return nil, err
	}
	evidenceDigest, err := credential.EvidenceDigest(evidence)
	if err != nil {
		return nil, err
	}
	challenge := fixture32(221)
	status := credential.LegacyCredentialStatus{RealmID: publication.RealmID, AccountID: publication.AccountID, Protocol: credential.ProtocolLegacy, CredentialGeneration: publication.CredentialGeneration, PublicationSequence: publication.PublicationSequence, IssuedAtMs: 1_700_000_000_010, ValidUntilMs: 1_700_000_300_010, PublicationDigest: publicationDigest, PublicationEvidenceDigest: evidenceDigest, Challenge: challenge, DelegationGeneration: 2, DelegationSerial: 4}
	statusRaw, err := status.Encode()
	if err != nil {
		return nil, err
	}
	point, signature := fixtureSignature(publicationRaw)
	envelope := credential.SignedCredential{Type: credential.SignedTypePublication, Inner: publicationRaw, Signature: signature}
	envelopeRaw, err := envelope.Encode()
	if err != nil {
		return nil, err
	}
	badEnvelope := []byte(`["concord-account-authority",2,"credential-signed","legacy-credential-status","` + base64.StdEncoding.EncodeToString(publicationRaw) + `","` + base64.StdEncoding.EncodeToString(signature[:]) + `"]`)
	nonCanonicalPublication := append([]byte(" "), publicationRaw...)
	unknownProtocolPublication := bytes.Replace(publicationRaw, []byte(`"legacy"`), []byte(`"unknown"`), 1)
	shortEpochDigestPublication := bytes.Replace(publicationRaw, []byte(b64(publication.CredentialEpochDigest[:])), []byte(b64(make([]byte, 47))), 1)
	invalidLifetimeStatus := bytes.Replace(statusRaw, []byte("1700000300010"), []byte("1700000300009"), 1)
	unknownProtocolStatus := bytes.Replace(statusRaw, []byte(`"legacy"`), []byte(`"split-pending"`), 1)
	shortChallengeStatus := bytes.Replace(statusRaw, []byte(b64(challenge[:])), []byte(b64(challenge[:31])), 1)
	shortSignatureEnvelope := bytes.Replace(envelopeRaw, []byte(b64(signature[:])), []byte(b64(signature[:95])), 1)
	return []vector{
		{Name: "publication/base", Suite: "publication", Operation: "decode", Input: map[string]any{"raw_b64": b64(publicationRaw)}, Expect: map[string]any{"verdict": "accept", "digest_b64": b64(publicationDigest[:])}},
		{Name: "publication/reject-noncanonical-whitespace", Suite: "publication", Operation: "decode", Input: map[string]any{"raw_b64": b64(nonCanonicalPublication)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
		{Name: "publication/reject-unknown-protocol", Suite: "publication", Operation: "decode", Input: map[string]any{"raw_b64": b64(unknownProtocolPublication)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
		{Name: "publication/reject-short-epoch-digest", Suite: "publication", Operation: "decode", Input: map[string]any{"raw_b64": b64(shortEpochDigestPublication)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
		{Name: "evidence/base", Suite: "evidence", Operation: "decode", Input: map[string]any{"raw_b64": b64(evidenceRaw)}, Expect: map[string]any{"verdict": "accept", "digest_b64": b64(evidenceDigest[:])}},
		{Name: "status/base", Suite: "status", Operation: "decode", Input: map[string]any{"raw_b64": b64(statusRaw), "challenge_b64": b64(challenge[:])}, Expect: map[string]any{"verdict": "accept"}},
		{Name: "status/reject-invalid-lifetime", Suite: "status", Operation: "decode", Input: map[string]any{"raw_b64": b64(invalidLifetimeStatus)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
		{Name: "status/reject-unknown-protocol", Suite: "status", Operation: "decode", Input: map[string]any{"raw_b64": b64(unknownProtocolStatus)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
		{Name: "status/reject-short-challenge", Suite: "status", Operation: "decode", Input: map[string]any{"raw_b64": b64(shortChallengeStatus)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
		{Name: "envelope/publication-valid-p384", Suite: "envelope", Operation: "verify", Input: map[string]any{"raw_b64": b64(envelopeRaw), "point_b64": b64(point)}, Expect: map[string]any{"verdict": "accept"}},
		{Name: "envelope/inner-type-mismatch", Suite: "envelope", Operation: "decode", Input: map[string]any{"raw_b64": b64(badEnvelope)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
		{Name: "envelope/reject-short-signature", Suite: "envelope", Operation: "decode", Input: map[string]any{"raw_b64": b64(shortSignatureEnvelope)}, Expect: map[string]any{"verdict": "reject", "reason": "malformed"}},
	}, nil
}

func fixturePublication() credential.CredentialPublication {
	return credential.CredentialPublication{RealmID: "11111111-1111-4111-8111-111111111111", AccountID: "22222222-2222-4222-8222-222222222222", Protocol: credential.ProtocolLegacy, CredentialGeneration: 7, PublicationSequence: 1, CredentialEpochDigest: fixture48(1), TransitionRequestDigest: fixture48(49), AuthorityChainSequence: 3, AuthorityChainHeadDigest: fixture48(97), IssuedAtMs: 1_700_000_000_000}
}

func fixtureEvidence(publicationDigest [48]byte, publication credential.CredentialPublication) credential.CredentialPublicationEvidence {
	return credential.CredentialPublicationEvidence{PublicationDigest: publicationDigest, DirectoryAdapterID: "akd-v0.13", RealmID: publication.RealmID, AccountID: publication.AccountID, LookupProofDigest: fixture48(11), HistoryOrConsistencyProofDigest: fixture48(59), CheckpointDigest: fixture48(107), CheckpointSize: 9, CheckpointIssuedAtMs: 1_700_000_000_001, ObserverQuorumDigest: fixture48(155), ObservedAtMs: 1_700_000_000_002, ValidUntilMs: 1_700_000_300_002}
}

func fixture48(start byte) [48]byte {
	var out [48]byte
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
}
func fixture32(start byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
}
func b64(value []byte) string { return base64.StdEncoding.EncodeToString(value) }
func sha384Hex(value []byte) string {
	digest := sha512.Sum384(value)
	return hex.EncodeToString(digest[:])
}

func fixtureSignature(message []byte) ([]byte, [96]byte) {
	curve := elliptic.P384()
	order := curve.Params().N
	d := big.NewInt(1)
	k := big.NewInt(2)
	x, y := curve.Params().Gx, curve.Params().Gy
	point := make([]byte, 97)
	point[0] = 4
	x.FillBytes(point[1:49])
	y.FillBytes(point[49:])
	//nolint:staticcheck // Test-only fixed-nonce fixture needs the affine x-coordinate of 2G.
	baseX, _ := curve.Add(x, y, x, y)
	r := new(big.Int).Mod(baseX, order)
	hash := sha512.Sum384(message)
	s := new(big.Int).Mul(r, d)
	s.Add(s, new(big.Int).SetBytes(hash[:]))
	s.Mul(s, new(big.Int).ModInverse(k, order))
	s.Mod(s, order)
	var signature [96]byte
	r.FillBytes(signature[:48])
	s.FillBytes(signature[48:])
	return point, signature
}
