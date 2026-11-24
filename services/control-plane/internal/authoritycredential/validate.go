package authoritycredential

import (
	"bytes"

	"github.com/google/uuid"
)

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func safe(value uint64) bool { return value <= maxSafeInt }

func positive(value uint64) bool { return value > 0 && value <= maxSafeInt }

func validatePublication(v CredentialPublication) error {
	if !canonicalUUID(v.RealmID) || !canonicalUUID(v.AccountID) || !safe(v.CredentialGeneration) || !positive(v.PublicationSequence) || !safe(v.AuthorityChainSequence) || !safe(v.IssuedAtMs) {
		return malformed("publication fields")
	}
	switch v.Protocol {
	case ProtocolLegacy:
		if v.DescriptorDigest.Present {
			return malformed("legacy descriptor")
		}
	case ProtocolSplitPending, ProtocolSplitAuthWrap:
		if !v.DescriptorDigest.Present {
			return malformed("split descriptor")
		}
	default:
		return malformed("protocol")
	}
	if v.PublicationSequence == 1 && v.PreviousPublicationDigest.Present {
		return malformed("first predecessor")
	}
	if v.PublicationSequence > 1 && !v.PreviousPublicationDigest.Present {
		return malformed("predecessor")
	}
	return nil
}

func validateStatus(v LegacyCredentialStatus) error {
	if !canonicalUUID(v.RealmID) || !canonicalUUID(v.AccountID) || v.Protocol != ProtocolLegacy || !safe(v.CredentialGeneration) || !positive(v.PublicationSequence) || !safe(v.IssuedAtMs) || !safe(v.DelegationGeneration) || !positive(v.DelegationSerial) {
		return malformed("status fields")
	}
	if v.IssuedAtMs > maxSafeInt-statusLifetimeMs || v.ValidUntilMs != v.IssuedAtMs+statusLifetimeMs {
		return malformed("status lifetime")
	}
	return nil
}

func validateEvidence(v CredentialPublicationEvidence) error {
	if !canonicalUUID(v.RealmID) || !canonicalUUID(v.AccountID) || !safe(v.CheckpointSize) || !safe(v.CheckpointIssuedAtMs) || !safe(v.ObservedAtMs) || !safe(v.ValidUntilMs) {
		return malformed("evidence fields")
	}
	return nil
}

func validateEnvelope(v SignedCredential) error {
	if v.Type != SignedTypePublication && v.Type != SignedTypeLegacyStatus {
		return malformed("signed type")
	}
	if len(v.Inner) == 0 || len(v.Inner) > 16*1024 {
		return malformed("inner")
	}
	switch v.Type {
	case SignedTypePublication:
		if _, err := DecodePublication(v.Inner); err != nil {
			return malformed("publication inner")
		}
	case SignedTypeLegacyStatus:
		if _, err := DecodeLegacyStatus(v.Inner); err != nil {
			return malformed("status inner")
		}
	}
	return nil
}

func equalDigest(left, right [48]byte) bool { return bytes.Equal(left[:], right[:]) }
