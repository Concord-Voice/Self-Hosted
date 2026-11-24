package authoritycredential

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	authoritycontract "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/authoritycontract"
)

const (
	prefix           = "concord-account-authority"
	version          = uint64(2)
	maxSafeInt       = uint64(9_007_199_254_740_991)
	statusLifetimeMs = uint64(300_000)
	reencodeRule     = "re-encode"
)

// ErrMalformed means an input is not a canonical credential-proof v2 value.
// It never includes wire-derived data in its text.
var ErrMalformed = errors.New("authoritycredential: malformed")

func malformed(rule string) error { return fmt.Errorf("%w: %s", ErrMalformed, rule) }

// CredentialProtocol is the closed credential-state enum.
type CredentialProtocol string

const (
	// ProtocolLegacy is the legacy password-verifier credential protocol.
	ProtocolLegacy CredentialProtocol = "legacy"
	// ProtocolSplitPending is the irreversible fence before split credentials.
	ProtocolSplitPending CredentialProtocol = "split-pending"
	// ProtocolSplitAuthWrap is the split credential protocol.
	ProtocolSplitAuthWrap CredentialProtocol = "split-auth-wrap-v1"
)

// SignedType is the closed outer-envelope signed-type registry. It does not
// grant a signer scope; scope validation belongs to a later activation layer.
type SignedType string

const (
	// SignedTypePublication identifies a credential publication envelope.
	SignedTypePublication SignedType = "credential-publication"
	// SignedTypeLegacyStatus identifies a legacy credential-status envelope.
	SignedTypeLegacyStatus SignedType = "legacy-credential-status"
)

// OptionalDigest distinguishes a protocol-permitted empty digest from a
// malformed digest. Value is meaningful only when Present is true.
type OptionalDigest struct {
	Present bool
	Value   [48]byte
}

// CredentialPublication is the v2 immutable credential-state record.
type CredentialPublication struct {
	RealmID, AccountID        string
	Protocol                  CredentialProtocol
	CredentialGeneration      uint64
	PublicationSequence       uint64
	PreviousPublicationDigest OptionalDigest
	CredentialEpochDigest     [48]byte
	DescriptorDigest          OptionalDigest
	TransitionRequestDigest   [48]byte
	AuthorityChainSequence    uint64
	AuthorityChainHeadDigest  [48]byte
	IssuedAtMs                uint64
}

// LegacyCredentialStatus is the short-lived, challenge-bound legacy statement.
type LegacyCredentialStatus struct {
	RealmID, AccountID        string
	Protocol                  CredentialProtocol
	CredentialGeneration      uint64
	PublicationSequence       uint64
	IssuedAtMs, ValidUntilMs  uint64
	PublicationDigest         [48]byte
	PublicationEvidenceDigest [48]byte
	Challenge                 [32]byte
	DelegationGeneration      uint64
	DelegationSerial          uint64
}

// CredentialPublicationEvidence binds a publication to opaque #10 evidence.
// This package parses the carrier only; it does not trust a directory proof.
type CredentialPublicationEvidence struct {
	PublicationDigest, LookupProofDigest, HistoryOrConsistencyProofDigest [48]byte
	CheckpointDigest, ObserverQuorumDigest                                [48]byte
	DirectoryAdapterID, RealmID, AccountID                                string
	CheckpointSize, CheckpointIssuedAtMs, ObservedAtMs, ValidUntilMs      uint64
}

// SignedCredential contains an outer v2 signature carrier. Mathematical
// verification is explicit and does not authorize the signer.
type SignedCredential struct {
	Type      SignedType
	Inner     []byte
	Signature [96]byte
}

// Encode returns canonical credential-publication bytes.
func (v CredentialPublication) Encode() ([]byte, error) {
	if err := validatePublication(v); err != nil {
		return nil, err
	}
	return encodeArray([]any{prefix, version, string(SignedTypePublication), v.RealmID, v.AccountID, string(v.Protocol), v.CredentialGeneration, v.PublicationSequence, optionalDigestText(v.PreviousPublicationDigest), digestText(v.CredentialEpochDigest), optionalDigestText(v.DescriptorDigest), digestText(v.TransitionRequestDigest), v.AuthorityChainSequence, digestText(v.AuthorityChainHeadDigest), v.IssuedAtMs})
}

// DecodePublication decodes a canonical publication and rejects every other
// JSON representation, including whitespace or escape sequences.
func DecodePublication(raw []byte) (CredentialPublication, error) {
	values, err := decodeArray(raw, string(SignedTypePublication), 15)
	if err != nil {
		return CredentialPublication{}, err
	}
	var v CredentialPublication
	if v.RealmID, err = stringAt(values, 3); err != nil {
		return v, err
	}
	if v.AccountID, err = stringAt(values, 4); err != nil {
		return v, err
	}
	protocol, err := stringAt(values, 5)
	if err != nil {
		return v, err
	}
	v.Protocol = CredentialProtocol(protocol)
	if v.CredentialGeneration, err = uintAt(values, 6); err != nil {
		return v, err
	}
	if v.PublicationSequence, err = uintAt(values, 7); err != nil {
		return v, err
	}
	if v.PreviousPublicationDigest, err = optionalDigestAt(values, 8); err != nil {
		return v, err
	}
	if v.CredentialEpochDigest, err = digestAt(values, 9); err != nil {
		return v, err
	}
	if v.DescriptorDigest, err = optionalDigestAt(values, 10); err != nil {
		return v, err
	}
	if v.TransitionRequestDigest, err = digestAt(values, 11); err != nil {
		return v, err
	}
	if v.AuthorityChainSequence, err = uintAt(values, 12); err != nil {
		return v, err
	}
	if v.AuthorityChainHeadDigest, err = digestAt(values, 13); err != nil {
		return v, err
	}
	if v.IssuedAtMs, err = uintAt(values, 14); err != nil {
		return v, err
	}
	if err = validatePublication(v); err != nil {
		return CredentialPublication{}, err
	}
	encoded, _ := v.Encode()
	if !bytes.Equal(encoded, raw) {
		return CredentialPublication{}, malformed(reencodeRule)
	}
	return v, nil
}

// Encode returns canonical legacy-credential-status bytes.
func (v LegacyCredentialStatus) Encode() ([]byte, error) {
	if err := validateStatus(v); err != nil {
		return nil, err
	}
	return encodeArray([]any{prefix, version, string(SignedTypeLegacyStatus), v.RealmID, v.AccountID, string(v.Protocol), v.CredentialGeneration, v.PublicationSequence, v.IssuedAtMs, v.ValidUntilMs, digestText(v.PublicationDigest), digestText(v.PublicationEvidenceDigest), base64.StdEncoding.EncodeToString(v.Challenge[:]), v.DelegationGeneration, v.DelegationSerial})
}

// DecodeLegacyStatus decodes a canonical, untrusted legacy-status carrier.
func DecodeLegacyStatus(raw []byte) (LegacyCredentialStatus, error) {
	values, err := decodeArray(raw, string(SignedTypeLegacyStatus), 15)
	if err != nil {
		return LegacyCredentialStatus{}, err
	}
	var v LegacyCredentialStatus
	if v.RealmID, err = stringAt(values, 3); err != nil {
		return v, err
	}
	if v.AccountID, err = stringAt(values, 4); err != nil {
		return v, err
	}
	p, err := stringAt(values, 5)
	if err != nil {
		return v, err
	}
	v.Protocol = CredentialProtocol(p)
	if v.CredentialGeneration, err = uintAt(values, 6); err != nil {
		return v, err
	}
	if v.PublicationSequence, err = uintAt(values, 7); err != nil {
		return v, err
	}
	if v.IssuedAtMs, err = uintAt(values, 8); err != nil {
		return v, err
	}
	if v.ValidUntilMs, err = uintAt(values, 9); err != nil {
		return v, err
	}
	if v.PublicationDigest, err = digestAt(values, 10); err != nil {
		return v, err
	}
	if v.PublicationEvidenceDigest, err = digestAt(values, 11); err != nil {
		return v, err
	}
	challenge, err := bytesAt(values, 12, 32)
	if err != nil {
		return v, err
	}
	copy(v.Challenge[:], challenge)
	if v.DelegationGeneration, err = uintAt(values, 13); err != nil {
		return v, err
	}
	if v.DelegationSerial, err = uintAt(values, 14); err != nil {
		return v, err
	}
	if err = validateStatus(v); err != nil {
		return LegacyCredentialStatus{}, err
	}
	encoded, _ := v.Encode()
	if !bytes.Equal(encoded, raw) {
		return LegacyCredentialStatus{}, malformed(reencodeRule)
	}
	return v, nil
}

// Encode returns canonical publication-evidence carrier bytes.
func (v CredentialPublicationEvidence) Encode() ([]byte, error) {
	if err := validateEvidence(v); err != nil {
		return nil, err
	}
	return encodeArray([]any{prefix, version, "credential-publication-evidence", digestText(v.PublicationDigest), v.DirectoryAdapterID, v.RealmID, v.AccountID, digestText(v.LookupProofDigest), digestText(v.HistoryOrConsistencyProofDigest), digestText(v.CheckpointDigest), v.CheckpointSize, v.CheckpointIssuedAtMs, digestText(v.ObserverQuorumDigest), v.ObservedAtMs, v.ValidUntilMs})
}

// DecodeEvidenceCarrier decodes a canonical but still untrusted evidence carrier.
func DecodeEvidenceCarrier(raw []byte) (CredentialPublicationEvidence, error) {
	values, err := decodeArray(raw, "credential-publication-evidence", 15)
	if err != nil {
		return CredentialPublicationEvidence{}, err
	}
	var v CredentialPublicationEvidence
	if v.PublicationDigest, err = digestAt(values, 3); err != nil {
		return v, err
	}
	if v.DirectoryAdapterID, err = stringAt(values, 4); err != nil {
		return v, err
	}
	if v.RealmID, err = stringAt(values, 5); err != nil {
		return v, err
	}
	if v.AccountID, err = stringAt(values, 6); err != nil {
		return v, err
	}
	if v.LookupProofDigest, err = digestAt(values, 7); err != nil {
		return v, err
	}
	if v.HistoryOrConsistencyProofDigest, err = digestAt(values, 8); err != nil {
		return v, err
	}
	if v.CheckpointDigest, err = digestAt(values, 9); err != nil {
		return v, err
	}
	if v.CheckpointSize, err = uintAt(values, 10); err != nil {
		return v, err
	}
	if v.CheckpointIssuedAtMs, err = uintAt(values, 11); err != nil {
		return v, err
	}
	if v.ObserverQuorumDigest, err = digestAt(values, 12); err != nil {
		return v, err
	}
	if v.ObservedAtMs, err = uintAt(values, 13); err != nil {
		return v, err
	}
	if v.ValidUntilMs, err = uintAt(values, 14); err != nil {
		return v, err
	}
	if err = validateEvidence(v); err != nil {
		return CredentialPublicationEvidence{}, err
	}
	encoded, _ := v.Encode()
	if !bytes.Equal(encoded, raw) {
		return CredentialPublicationEvidence{}, malformed(reencodeRule)
	}
	return v, nil
}

// Encode returns canonical credential-signed bytes. This function does not sign.
func (v SignedCredential) Encode() ([]byte, error) {
	if err := validateEnvelope(v); err != nil {
		return nil, err
	}
	return encodeArray([]any{prefix, version, "credential-signed", string(v.Type), base64.StdEncoding.EncodeToString(v.Inner), base64.StdEncoding.EncodeToString(v.Signature[:])})
}

// DecodeSignedEnvelope decodes only publication and legacy-status envelopes.
func DecodeSignedEnvelope(raw []byte) (SignedCredential, error) {
	values, err := decodeArray(raw, "credential-signed", 6)
	if err != nil {
		return SignedCredential{}, err
	}
	var v SignedCredential
	typ, err := stringAt(values, 3)
	if err != nil {
		return v, err
	}
	v.Type = SignedType(typ)
	if v.Inner, err = bytesUpToAt(values, 4, 16*1024); err != nil {
		return v, err
	}
	sig, err := bytesAt(values, 5, 96)
	if err != nil {
		return v, err
	}
	copy(v.Signature[:], sig)
	if err = validateEnvelope(v); err != nil {
		return SignedCredential{}, err
	}
	encoded, _ := v.Encode()
	if !bytes.Equal(encoded, raw) {
		return SignedCredential{}, malformed(reencodeRule)
	}
	return v, nil
}

// PublicationDigest returns SHA-384 of canonical publication bytes.
func PublicationDigest(v CredentialPublication) ([48]byte, error) {
	raw, err := v.Encode()
	return digestRaw(raw, err)
}

// EvidenceDigest returns SHA-384 of canonical evidence bytes.
func EvidenceDigest(v CredentialPublicationEvidence) ([48]byte, error) {
	raw, err := v.Encode()
	return digestRaw(raw, err)
}

// VerifyP384Signature checks mathematical P-384/P1363 validity only. It does
// not validate a delegation, scope, authority root, or eligibility.
func VerifyP384Signature(point, canonicalInner, signature []byte) bool {
	return authoritycontract.VerifyP384(point, canonicalInner, signature)
}

func digestRaw(raw []byte, err error) ([48]byte, error) {
	if err != nil {
		return [48]byte{}, err
	}
	return sha512.Sum384(raw), nil
}

func encodeArray(values []any) ([]byte, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, malformed("encode")
	}
	if !allowedBytes(encoded) {
		return nil, malformed("encoding")
	}
	return encoded, nil
}

func decodeArray(raw []byte, wantTag string, wantLen int) ([]any, error) {
	if len(raw) == 0 || len(raw) > 16*1024 || !allowedBytes(raw) {
		return nil, malformed("encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var values []any
	if err := dec.Decode(&values); err != nil {
		return nil, malformed("json")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, malformed("trailing")
	}
	if len(values) != wantLen {
		return nil, malformed("field count")
	}
	first, err := stringAt(values, 0)
	if err != nil || first != prefix {
		return nil, malformed("prefix")
	}
	v, err := uintAt(values, 1)
	if err != nil || v != version {
		return nil, malformed("version")
	}
	tag, err := stringAt(values, 2)
	if err != nil || tag != wantTag {
		return nil, malformed("tag")
	}
	return values, nil
}

func allowedBytes(raw []byte) bool {
	for _, b := range raw {
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '[' && b != ']' && b != ',' && b != '"' && b != ':' && b != '-' && b != '_' && b != '.' && b != '/' && b != '+' && b != '=' {
			return false
		}
	}
	return true
}

func stringAt(values []any, index int) (string, error) {
	value, ok := values[index].(string)
	if !ok || value == "" {
		return "", malformed("string")
	}
	return value, nil
}
func uintAt(values []any, index int) (uint64, error) {
	number, ok := values[index].(json.Number)
	if !ok {
		return 0, malformed("integer")
	}
	text := number.String()
	if text == "" || (len(text) > 1 && text[0] == '0') {
		return 0, malformed("integer")
	}
	for i := range text {
		if text[i] < '0' || text[i] > '9' {
			return 0, malformed("integer")
		}
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value > maxSafeInt {
		return 0, malformed("integer")
	}
	return value, nil
}
func bytesAt(values []any, index, size int) ([]byte, error) {
	text, err := stringAt(values, index)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != text || len(decoded) != size {
		return nil, malformed("base64")
	}
	return decoded, nil
}

func bytesUpToAt(values []any, index, maximum int) ([]byte, error) {
	text, err := stringAt(values, index)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != text || len(decoded) == 0 || len(decoded) > maximum {
		return nil, malformed("base64")
	}
	return decoded, nil
}
func digestAt(values []any, index int) ([48]byte, error) {
	var out [48]byte
	decoded, err := bytesAt(values, index, 48)
	if err != nil {
		return out, err
	}
	copy(out[:], decoded)
	return out, nil
}
func optionalDigestAt(values []any, index int) (OptionalDigest, error) {
	text, ok := values[index].(string)
	if !ok {
		return OptionalDigest{}, malformed("optional digest")
	}
	if text == "" {
		return OptionalDigest{}, nil
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != text || len(decoded) != 48 {
		return OptionalDigest{}, malformed("optional digest")
	}
	var out OptionalDigest
	out.Present = true
	copy(out.Value[:], decoded)
	return out, nil
}
func digestText(value [48]byte) string { return base64.StdEncoding.EncodeToString(value[:]) }
func optionalDigestText(value OptionalDigest) string {
	if !value.Present {
		return ""
	}
	return digestText(value.Value)
}
