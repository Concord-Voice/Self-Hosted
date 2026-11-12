package main

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// selfhost-authority.sh writes the first delegation with values of its own:
// the empty population's digest for seal-legacy, the full scope, and the
// lifetime. This keeps them equal to the contract.
func TestSelfhostAuthorityScriptMatchesTheContract(t *testing.T) {
	src, err := os.ReadFile("../../../../infrastructure/deploy/selfhost-authority.sh")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("selfhost-authority.sh is not in this tree")
	}
	require.NoError(t, err)
	// A quoted value may hold spaces; an unquoted one ends at whitespace.
	value := func(name string) string {
		m := regexp.MustCompile(`(?m)\b` + name + `=(?:'([^']*)'|(\S+))`).FindSubmatch(src)
		require.NotNil(t, m, name)
		return string(m[1]) + string(m[2])
	}
	ms := func(name string) uint64 {
		v, err := strconv.ParseUint(value(name), 10, 64)
		require.NoError(t, err, name)
		return v
	}

	empty, err := ac.PopulationDigest(nil)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(empty), value("AUTHORITY_EMPTY_POPULATION_DIGEST"))

	all := []string{ac.OpAddDevice, ac.OpBindRecovery, ac.OpCancelPending, ac.OpCancelRecoveryBind,
		ac.OpEnrollExisting, ac.OpEnrollNew, ac.OpErase, ac.OpRecover, ac.OpReplaceRoot, ac.OpRevokeRoot,
		ac.OpRotateEK, ac.OpRotateRoot, ac.ScopeStatus, ac.OpSuspend, ac.OpUnsuspend}
	slices.Sort(all)
	require.Equal(t, all, strings.Fields(value("AUTHORITY_SCOPE")))

	// §2.5's bounds, as delegationBoundsOK checks them.
	require.LessOrEqual(t, ms("AUTHORITY_LIFETIME_MS"), ac.DelegationLifetimeMaxMs)
	tau, delta, g := ms("AUTHORITY_TAU_MS"), ms("AUTHORITY_DELTA_MS"), ms("AUTHORITY_G_MS")
	require.True(t, tau >= 1 && tau <= ac.VMaxMs, "tau")
	require.True(t, delta > ac.IntentWindowMs+ac.EpsilonMs && delta <= ac.DeltaMaxMs, "delta")
	require.True(t, g >= 1 && g <= delta, "g")
}
