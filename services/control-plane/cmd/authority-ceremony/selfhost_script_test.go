package main

import (
	"encoding/hex"
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
	require.NoError(t, err)
	value := func(name string) string {
		m := regexp.MustCompile(`(?m)\b` + name + `='?([^'\s]+(?: [^'\s]+)*)'?`).FindSubmatch(src)
		require.NotNil(t, m, name)
		return string(m[1])
	}

	empty, err := ac.PopulationDigest(nil)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(empty), value("AUTHORITY_EMPTY_POPULATION_DIGEST"))

	all := []string{ac.OpAddDevice, ac.OpBindRecovery, ac.OpCancelPending, ac.OpCancelRecoveryBind,
		ac.OpEnrollExisting, ac.OpEnrollNew, ac.OpErase, ac.OpRecover, ac.OpReplaceRoot, ac.OpRevokeRoot,
		ac.OpRotateEK, ac.OpRotateRoot, ac.ScopeStatus, ac.OpSuspend, ac.OpUnsuspend}
	slices.Sort(all)
	require.Equal(t, all, strings.Fields(value("AUTHORITY_SCOPE")))

	lifetime, err := strconv.ParseUint(value("AUTHORITY_LIFETIME_MS"), 10, 64)
	require.NoError(t, err)
	require.LessOrEqual(t, lifetime, ac.DelegationLifetimeMaxMs)
}
