package auth

// signalMatch implements the absent-is-no-signal convention for nullable
// session-metadata columns (device_name / ip_address / user_agent / machine_id).
//
// A nullable metadata column has TWO meaningful Go states, not three. NULL and
// "" both mean absent. A DISPLAY reader renders absent as unknown. An EQUALITY
// reader treats absent as NO SIGNAL — never as a value, and never as a
// mismatch.
//
// known is true only when BOTH sides carry a value; ok is true when the signal
// is unknown OR the two sides agree.
//
// This is the EQUALITY treatment. A DISPLAY reader must NOT use it — it renders
// absent as "unknown" instead. Adding a nullable column? Pick one, deliberately.
//
// Introduced by #3290. Coalescing three nullable columns to "" at the readers
// would otherwise have made every absent value compare as a MISMATCHED one,
// with two consequences: every benign SSO rotation race would emit a
// SeverityHigh refresh-replay event, and checkMachineIDTheft would escalate an
// uncorroborated machine-ID mismatch to a bulk session revocation. The shape is
// not new — machineIDOk at the grace gate was already this expression, written
// out by hand — this names it so the next nullable column inherits it rather
// than re-deriving it.
func signalMatch(stored, presented string) (known, ok bool) {
	known = stored != "" && presented != ""
	return known, !known || stored == presented
}
