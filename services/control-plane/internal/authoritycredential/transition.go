package authoritycredential

// CheckPublicationSuccessor validates only immutable publication-chain shape.
// It neither persists nor authorizes a transition.
func CheckPublicationSuccessor(previous, next CredentialPublication) error {
	if err := validatePublication(previous); err != nil {
		return err
	}
	if err := validatePublication(next); err != nil {
		return err
	}
	if err := checkSuccessorIdentity(previous, next); err != nil {
		return err
	}
	if err := checkSuccessorSequence(previous, next); err != nil {
		return err
	}
	if err := checkSuccessorPredecessor(previous, next); err != nil {
		return err
	}
	if err := checkAuthorityChain(previous, next); err != nil {
		return err
	}
	return checkProtocolTransition(previous, next)
}

func checkSuccessorIdentity(previous, next CredentialPublication) error {
	if previous.RealmID != next.RealmID || previous.AccountID != next.AccountID {
		return malformed("successor identity")
	}
	return nil
}

func checkSuccessorSequence(previous, next CredentialPublication) error {
	if previous.PublicationSequence == maxSafeInt || next.PublicationSequence != previous.PublicationSequence+1 {
		return malformed("successor sequence")
	}
	return nil
}

func checkSuccessorPredecessor(previous, next CredentialPublication) error {
	previousDigest, err := PublicationDigest(previous)
	if err != nil {
		return err
	}
	if !next.PreviousPublicationDigest.Present || !equalDigest(next.PreviousPublicationDigest.Value, previousDigest) {
		return malformed("successor predecessor")
	}
	return nil
}

func checkAuthorityChain(previous, next CredentialPublication) error {
	if next.AuthorityChainSequence < previous.AuthorityChainSequence || (next.AuthorityChainSequence == previous.AuthorityChainSequence && !equalDigest(next.AuthorityChainHeadDigest, previous.AuthorityChainHeadDigest)) {
		return malformed("authority chain")
	}
	return nil
}

func checkProtocolTransition(previous, next CredentialPublication) error {
	switch previous.Protocol {
	case ProtocolLegacy:
		if next.Protocol == ProtocolLegacy && next.CredentialGeneration <= previous.CredentialGeneration {
			return malformed("legacy generation")
		}
		if next.Protocol == ProtocolSplitPending && (previous.CredentialGeneration == maxSafeInt || next.CredentialGeneration != previous.CredentialGeneration+1) {
			return malformed("split pending generation")
		}
		if next.Protocol != ProtocolLegacy && next.Protocol != ProtocolSplitPending {
			return malformed("legacy transition")
		}
	case ProtocolSplitPending:
		if next.Protocol != ProtocolSplitAuthWrap || next.CredentialGeneration != previous.CredentialGeneration {
			return malformed("split pending transition")
		}
	case ProtocolSplitAuthWrap:
		if next.Protocol != ProtocolSplitAuthWrap || next.CredentialGeneration <= previous.CredentialGeneration {
			return malformed("split generation")
		}
	default:
		return malformed("protocol")
	}
	return nil
}

// CheckStaticStatusBindings proves byte-level relationships only. A nil error
// is never an eligibility, scope, freshness, or authority decision.
func CheckStaticStatusBindings(status LegacyCredentialStatus, publication CredentialPublication, evidence CredentialPublicationEvidence, challenge [32]byte) error {
	if err := validateStatus(status); err != nil {
		return err
	}
	if err := validatePublication(publication); err != nil {
		return err
	}
	if err := validateEvidence(evidence); err != nil {
		return err
	}
	if publication.Protocol != ProtocolLegacy || status.RealmID != publication.RealmID || status.AccountID != publication.AccountID || status.CredentialGeneration != publication.CredentialGeneration || status.PublicationSequence != publication.PublicationSequence || status.RealmID != evidence.RealmID || status.AccountID != evidence.AccountID || status.Challenge != challenge {
		return malformed("status binding")
	}
	publicationDigest, err := PublicationDigest(publication)
	if err != nil {
		return err
	}
	evidenceDigest, err := EvidenceDigest(evidence)
	if err != nil {
		return err
	}
	if !equalDigest(status.PublicationDigest, publicationDigest) || !equalDigest(status.PublicationEvidenceDigest, evidenceDigest) || !equalDigest(evidence.PublicationDigest, publicationDigest) {
		return malformed("status digest")
	}
	return nil
}
