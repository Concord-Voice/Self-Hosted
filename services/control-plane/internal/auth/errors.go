package auth

// This file declares sentinel errors exposed to other packages.
//
// These sentinels let cross-package callers (notably internal/oauth, which
// shares the password-verification path via AuthAdapter) discriminate failure
// modes via errors.Is rather than fragile string matching against err.Error().

import "errors"

// ErrAccountLocked is returned by VerifyPassword when the per-email lockout
// counter has reached the threshold defined by loginLockoutThreshold /
// loginLockoutDurations. Callers should translate to HTTP 423 Locked.
//
// Used by:
//   - internal/oauth.Handler.CompleteLink — translates to 423 + error_code
//     "account_locked" so the renderer can surface a dedicated UX state
//     distinct from generic 401 invalid_credentials.
var ErrAccountLocked = errors.New("account_locked: too many failed attempts")
