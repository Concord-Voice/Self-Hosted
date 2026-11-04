package accountguard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

var errRequestShape = errors.New("accountguard: invalid request component")

type requestFingerprintInput struct {
	kind           decide.ReqKind
	targets        []decide.CredRef
	resetFactors   bool
	pendingAddress string
}

func validRequestRef(ref decide.CredRef) bool {
	return ref.Kind >= decide.CredPassword && ref.Kind <= decide.CredRecoveryDevice &&
		ref.ID != "" && utf8.ValidString(ref.ID) && !strings.ContainsRune(ref.ID, 0) &&
		uint64(len(ref.ID)) <= math.MaxUint32 && ref.Version > 0
}

func requestRefLess(a, b decide.CredRef) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	return a.Version < b.Version
}

func validRequestRefs(refs []decide.CredRef) bool {
	if uint64(len(refs)) > math.MaxUint32 {
		return false
	}
	for i, ref := range refs {
		if !validRequestRef(ref) || (i > 0 && !requestRefLess(refs[i-1], ref)) {
			return false
		}
	}
	return true
}

func validRequestTargets(kind decide.ReqKind, targets []decide.CredRef, reset bool) bool {
	if !validRequestRefs(targets) || (reset && kind != decide.ReqBreakGlass) {
		return false
	}
	switch kind {
	case decide.ReqBreakGlass:
		return len(targets) == 0
	case decide.ReqAddressChange:
		return len(targets) == 1 && (targets[0].Kind == decide.CredAddress || targets[0].Kind == decide.CredBackupAddress)
	case decide.ReqCredentialRemove:
		return len(targets) == 1 && targets[0].Kind != decide.CredPassword && targets[0].Kind != decide.CredAddress
	default:
		return false
	}
}

func normalizedRequestAddress(s string) bool {
	return s != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0) &&
		uint64(len(s)) <= math.MaxUint32 && strings.ToLower(strings.TrimSpace(s)) == s
}

func appendRequestText(dst []byte, value string) ([]byte, error) {
	length := uint64(len(value))
	if length > math.MaxUint32 {
		return nil, errRequestShape
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(length))
	return append(dst, value...), nil
}

func appendRequestRefs(dst []byte, refs []decide.CredRef) ([]byte, error) {
	length := uint64(len(refs))
	if length > math.MaxUint32 {
		return nil, errRequestShape
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(length))
	for _, ref := range refs {
		if ref.Version <= 0 {
			return nil, errRequestShape
		}
		dst = append(dst, byte(ref.Kind))
		var err error
		dst, err = appendRequestText(dst, ref.ID)
		if err != nil {
			return nil, err
		}
		dst = binary.BigEndian.AppendUint64(dst, uint64(ref.Version))
	}
	return dst, nil
}

func originFingerprint(refs []decide.CredRef) ([32]byte, error) {
	if len(refs) == 0 || !validRequestRefs(refs) {
		return [32]byte{}, errRequestShape
	}
	transcript, err := appendRequestRefs([]byte("concord/accountguard/origin/v1\x00"), refs)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(transcript), nil
}

// requestFingerprints consumes an already-derived key, never allocates a key
// generation, and deliberately excludes proof origins and proposed passwords.
func requestFingerprints(input requestFingerprintInput, key []byte, keyID int16) (decide.Candidate, error) {
	if len(key) != sha256.Size || keyID <= 0 || !validRequestTargets(input.kind, input.targets, input.resetFactors) {
		return decide.Candidate{}, errRequestShape
	}
	if input.kind == decide.ReqAddressChange {
		if !normalizedRequestAddress(input.pendingAddress) {
			return decide.Candidate{}, errRequestShape
		}
	} else if input.pendingAddress != "" {
		return decide.Candidate{}, errRequestShape
	}
	core, err := appendRequestRefs([]byte{byte(input.kind)}, input.targets)
	if err != nil {
		return decide.Candidate{}, err
	}
	var reset byte
	if input.resetFactors {
		reset = 1
	}
	core = append(core, reset)
	coarse := sha256.Sum256(append([]byte("concord/accountguard/request-coarse/v1\x00"), core...))
	keyed := append([]byte("concord/accountguard/request-keyed/v1\x00"), core...)
	keyed, err = appendRequestText(keyed, input.pendingAddress)
	if err != nil {
		return decide.Candidate{}, err
	}
	mac := hmac.New(sha256.New, key)
	if _, err := mac.Write(keyed); err != nil {
		return decide.Candidate{}, err
	}
	return decide.Candidate{Keyed: mac.Sum(nil), Coarse: coarse[:], KeyID: keyID}, nil
}
