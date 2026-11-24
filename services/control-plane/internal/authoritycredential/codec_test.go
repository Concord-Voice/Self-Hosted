package authoritycredential

import (
	"crypto/elliptic"
	"crypto/sha512"
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixtureDigest(start byte) [48]byte {
	var out [48]byte
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
}

func fixturePublication() CredentialPublication {
	return CredentialPublication{
		RealmID:                  "11111111-1111-4111-8111-111111111111",
		AccountID:                "22222222-2222-4222-8222-222222222222",
		Protocol:                 ProtocolLegacy,
		CredentialGeneration:     7,
		PublicationSequence:      1,
		CredentialEpochDigest:    fixtureDigest(1),
		TransitionRequestDigest:  fixtureDigest(49),
		AuthorityChainSequence:   3,
		AuthorityChainHeadDigest: fixtureDigest(97),
		IssuedAtMs:               1_700_000_000_000,
	}
}

func fixtureEvidence(publication CredentialPublication) CredentialPublicationEvidence {
	digest, err := PublicationDigest(publication)
	if err != nil {
		panic(err)
	}
	return CredentialPublicationEvidence{
		PublicationDigest:               digest,
		DirectoryAdapterID:              "akd-v0.13",
		RealmID:                         publication.RealmID,
		AccountID:                       publication.AccountID,
		LookupProofDigest:               fixtureDigest(11),
		HistoryOrConsistencyProofDigest: fixtureDigest(59),
		CheckpointDigest:                fixtureDigest(107),
		CheckpointSize:                  9,
		CheckpointIssuedAtMs:            1_700_000_000_001,
		ObserverQuorumDigest:            fixtureDigest(155),
		ObservedAtMs:                    1_700_000_000_002,
		ValidUntilMs:                    1_700_000_300_002,
	}
}

func fixtureStatus(publication CredentialPublication, evidence CredentialPublicationEvidence, challenge [32]byte) LegacyCredentialStatus {
	publicationDigest, err := PublicationDigest(publication)
	if err != nil {
		panic(err)
	}
	evidenceDigest, err := EvidenceDigest(evidence)
	if err != nil {
		panic(err)
	}
	return LegacyCredentialStatus{
		RealmID:                   publication.RealmID,
		AccountID:                 publication.AccountID,
		Protocol:                  ProtocolLegacy,
		CredentialGeneration:      publication.CredentialGeneration,
		PublicationSequence:       publication.PublicationSequence,
		IssuedAtMs:                1_700_000_000_010,
		ValidUntilMs:              1_700_000_300_010,
		PublicationDigest:         publicationDigest,
		PublicationEvidenceDigest: evidenceDigest,
		Challenge:                 challenge,
		DelegationGeneration:      2,
		DelegationSerial:          4,
	}
}

func TestCanonicalRoundTripsAndStaticBindings(t *testing.T) {
	publication := fixturePublication()
	evidence := fixtureEvidence(publication)
	var challenge [32]byte
	for i := range challenge {
		challenge[i] = byte(i)
	}
	status := fixtureStatus(publication, evidence, challenge)

	publicationRaw, err := publication.Encode()
	require.NoError(t, err)
	decodedPublication, err := DecodePublication(publicationRaw)
	require.NoError(t, err)
	require.Equal(t, publication, decodedPublication)

	evidenceRaw, err := evidence.Encode()
	require.NoError(t, err)
	decodedEvidence, err := DecodeEvidenceCarrier(evidenceRaw)
	require.NoError(t, err)
	require.Equal(t, evidence, decodedEvidence)

	statusRaw, err := status.Encode()
	require.NoError(t, err)
	decodedStatus, err := DecodeLegacyStatus(statusRaw)
	require.NoError(t, err)
	require.Equal(t, status, decodedStatus)
	require.NoError(t, CheckStaticStatusBindings(status, publication, evidence, challenge))
}

func TestCodecRejectsNonCanonicalAndMismatchedInputs(t *testing.T) {
	publication := fixturePublication()
	raw, err := publication.Encode()
	require.NoError(t, err)
	_, err = DecodePublication(append([]byte(" "), raw...))
	require.ErrorIs(t, err, ErrMalformed)
	_, err = DecodePublication(append(append([]byte{}, raw...), []byte("[]")...))
	require.ErrorIs(t, err, ErrMalformed)

	publication.Protocol = ProtocolSplitAuthWrap
	_, err = publication.Encode()
	require.ErrorIs(t, err, ErrMalformed, "a split publication needs a descriptor digest")

	publication = fixturePublication()
	evidence := fixtureEvidence(publication)
	var challenge [32]byte
	status := fixtureStatus(publication, evidence, challenge)
	status.Challenge[0] = 1
	require.ErrorIs(t, CheckStaticStatusBindings(status, publication, evidence, challenge), ErrMalformed)
	status = fixtureStatus(publication, evidence, challenge)
	status.ValidUntilMs--
	_, err = status.Encode()
	require.ErrorIs(t, err, ErrMalformed)
	status = fixtureStatus(publication, evidence, challenge)
	status.DelegationGeneration = 0
	_, err = status.Encode()
	require.NoError(t, err)
}

func TestPublicationSuccessorRules(t *testing.T) {
	previous := fixturePublication()
	digest, err := PublicationDigest(previous)
	require.NoError(t, err)
	next := previous
	next.PublicationSequence = 2
	next.PreviousPublicationDigest = OptionalDigest{Present: true, Value: digest}
	next.CredentialGeneration = previous.CredentialGeneration + 1
	require.NoError(t, CheckPublicationSuccessor(previous, next))

	next.PublicationSequence = 3
	require.ErrorIs(t, CheckPublicationSuccessor(previous, next), ErrMalformed)
	next.PublicationSequence = 2
	next.Protocol = ProtocolSplitAuthWrap
	require.ErrorIs(t, CheckPublicationSuccessor(previous, next), ErrMalformed)
	next.Protocol = ProtocolSplitPending
	next.DescriptorDigest = OptionalDigest{Present: true, Value: fixtureDigest(201)}
	require.NoError(t, CheckPublicationSuccessor(previous, next))

	previous.CredentialGeneration = maxSafeInt
	digest, err = PublicationDigest(previous)
	require.NoError(t, err)
	next = previous
	next.PublicationSequence = 2
	next.PreviousPublicationDigest = OptionalDigest{Present: true, Value: digest}
	next.Protocol = ProtocolSplitPending
	next.DescriptorDigest = OptionalDigest{Present: true, Value: fixtureDigest(201)}
	require.ErrorIs(t, CheckPublicationSuccessor(previous, next), ErrMalformed)
}

func TestSignedEnvelopeChecksExactTypeAndP384Math(t *testing.T) {
	publication := fixturePublication()
	inner, err := publication.Encode()
	require.NoError(t, err)
	point, signature := fixtureSignature(inner)
	envelope := SignedCredential{Type: SignedTypePublication, Inner: inner, Signature: signature}
	raw, err := envelope.Encode()
	require.NoError(t, err)
	decoded, err := DecodeSignedEnvelope(raw)
	require.NoError(t, err)
	require.Equal(t, envelope, decoded)
	require.True(t, VerifyP384Signature(point, inner, signature[:]))
	signature[0] ^= 1
	require.False(t, VerifyP384Signature(point, inner, signature[:]))

	badType := SignedCredential{Type: SignedTypeLegacyStatus, Inner: inner}
	_, err = badType.Encode()
	require.ErrorIs(t, err, ErrMalformed)
}

// fixtureSignature produces a deterministic test-only P-384 P1363 signature.
// Neither its private scalar nor any signing API is exported by this package.
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
	z := new(big.Int).SetBytes(hash[:])
	s := new(big.Int).Mul(r, d)
	s.Add(s, z)
	s.Mul(s, new(big.Int).ModInverse(k, order))
	s.Mod(s, order)
	var signature [96]byte
	r.FillBytes(signature[:48])
	s.FillBytes(signature[48:])
	return point, signature
}

func TestEnvelopeRejectsNonCanonicalBase64(t *testing.T) {
	publication := fixturePublication()
	inner, err := publication.Encode()
	require.NoError(t, err)
	_, signature := fixtureSignature(inner)
	encoded := base64.StdEncoding.EncodeToString(inner)
	bad := []byte(`["concord-account-authority",2,"credential-signed","credential-publication","` + encoded + `\n","` + base64.StdEncoding.EncodeToString(signature[:]) + `"]`)
	_, err = DecodeSignedEnvelope(bad)
	require.ErrorIs(t, err, ErrMalformed)
}
