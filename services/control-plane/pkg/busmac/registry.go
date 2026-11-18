package busmac

// Family names one signed wire format on the bus. The same type names a raw
// purpose (see RawMAC): both are domain separators that select the subkey and
// lead the MAC input, and the registry below says which one a string is.
type Family string

// The families this package ships. Each is a direction (producer>consumer) and
// a wire format; the string is part of every MAC computed under it, so it can
// never be renamed without re-keying every sender and receiver.
const (
	FamilyLifecycle Family = "mp>cp/lifecycle"
	FamilyTelemetry Family = "mp>cp/telemetry"
)

type purposeKind uint8

const (
	// kindFamily is an enveloped family: Sign, Open, MAC and VerifyMAC accept
	// it, and RawMAC and RawVerify refuse it with ErrReservedPurpose.
	kindFamily purposeKind = iota + 1
	// kindRaw is a raw purpose: RawMAC and RawVerify accept it, and every
	// family operation refuses it as unregistered.
	kindRaw
)

// purposes is the single registry of every family and raw purpose. A string
// has exactly one kind, which is what keeps a raw caller from minting or
// accepting a family MAC: RawMAC over a family's own lines under that
// family's string would otherwise produce exactly that family's MAC.
//
// Security#175 adds its raw purposes and their header name here, as kindRaw
// entries. A new string must never reuse an existing one, in either kind.
var purposes = map[Family]purposeKind{
	FamilyLifecycle: kindFamily,
	FamilyTelemetry: kindFamily,
}

// Registered reports whether f is a registered family. A raw purpose is not a
// family, so Registered is false for it.
func Registered(f Family) bool {
	return purposes[f] == kindFamily
}
