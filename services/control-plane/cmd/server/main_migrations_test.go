package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
)

const (
	unavailableLine = "migrations status: unavailable\n"
	sentinelDSNMark = "SENTINEL-pw-7f3a91"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func neverCalledStatus(t *testing.T) func(context.Context, string) (migrationrunner.Status, error) {
	t.Helper()
	return func(context.Context, string) (migrationrunner.Status, error) {
		t.Fatal("status reader must not be called")
		return migrationrunner.Status{}, nil
	}
}

func TestDispatchRoutesMigrationsToItsRunner(t *testing.T) {
	var got []string
	runners := controlPlaneSubcommandRunners{
		migrations: func(args []string) int {
			got = args
			return 3
		},
	}
	code, handled := dispatchControlPlaneSubcommandWithRunners(
		[]string{"control-plane", "migrations", "status"}, runners,
	)
	// Mutation: drop the "migrations" case from dispatch (falls to usage exit 64).
	require.True(t, handled)
	require.Equal(t, 3, code)
	require.Equal(t, []string{"status"}, got)
}

func TestDispatchProductionWrapperRoutesMigrations(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	code, handled := dispatchControlPlaneSubcommand([]string{"control-plane", "migrations", "status"})
	// Mutation: leave the production wrapper without the migrations runner (nil call panics)
	// or route it to the unknown-subcommand usage path (64).
	require.True(t, handled)
	require.Equal(t, 1, code)
}

func TestMigrationsCtl_ExitCodeTable(t *testing.T) {
	env := envOf(map[string]string{"DATABASE_URL": "postgres://u:" + sentinelDSNMark + "@db.invalid/x"})
	cases := []struct {
		name string
		st   migrationrunner.Status
		line string
		code int
	}{
		{"current", migrationrunner.Status{Initialised: true, Version: 7, Latest: 7}, "current 7", 0},
		{"pending", migrationrunner.Status{Initialised: true, Version: 5, Latest: 7}, "pending 5 7", 3},
		{"ahead", migrationrunner.Status{Initialised: true, Version: 9, Latest: 7}, "ahead 9 7", 3},
		{"dirty", migrationrunner.Status{Initialised: true, Version: 7, Dirty: true, Latest: 7}, "dirty 7", 3},
		{"uninitialised", migrationrunner.Status{Latest: 7}, "uninitialised 7", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := runMigrationsCtlWith([]string{"status"}, env, &out, &errOut,
				func(context.Context, string) (migrationrunner.Status, error) { return tc.st, nil })
			// Mutation: exit 0 for every line, or 3 for current, or print anything but Line()+"\n".
			require.Equal(t, tc.code, code)
			require.Equal(t, tc.line+"\n", out.String())
			require.Empty(t, errOut.String())
		})
	}
}

func TestMigrationsCtl_UsageErrorsExit64(t *testing.T) {
	env := envOf(map[string]string{"DATABASE_URL": "postgres://u:" + sentinelDSNMark + "@db.invalid/x"})
	for name, args := range map[string][]string{
		"no verb":      nil,
		"unknown verb": {"apply"},
		"extra args":   {"status", "extra"},
		"empty verb":   {""},
		"verb in caps": {"Status"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := runMigrationsCtlWith(args, env, &out, &errOut, neverCalledStatus(t))
			// Mutation: ignore extra args / accept unknown verbs / read the database before parsing args.
			require.Equal(t, 64, code)
			require.Empty(t, out.String())
			require.NotContains(t, errOut.String(), sentinelDSNMark)
		})
	}
}

func TestMigrationsCtl_EmptyDatabaseURLIsFixedFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runMigrationsCtlWith([]string{"status"}, envOf(nil), &out, &errOut, neverCalledStatus(t))
	// Mutation: fall through to a default DSN, or word the stderr line differently, or print to stdout.
	require.Equal(t, 1, code)
	require.Equal(t, unavailableLine, errOut.String())
	require.Empty(t, out.String())
}

func TestMigrationsCtl_ReaderFailureNeverLeaksDetail(t *testing.T) {
	dsn := "postgres://admin:" + sentinelDSNMark + "@db.invalid:5432/app"
	var out, errOut bytes.Buffer
	code := runMigrationsCtlWith([]string{"status"}, envOf(map[string]string{"DATABASE_URL": dsn}), &out, &errOut,
		func(context.Context, string) (migrationrunner.Status, error) {
			return migrationrunner.Status{}, errors.New("dial " + dsn + ": refused")
		})
	// Mutation: print the wrapped error (or a prefix of it) to stderr.
	require.Equal(t, 1, code)
	require.Equal(t, unavailableLine, errOut.String())
	require.Empty(t, out.String())
}

func TestMigrationsCtl_UnreachableDatabaseLeaksNoDSN(t *testing.T) {
	dsn := "postgres://admin:" + sentinelDSNMark + "@127.0.0.1:1/app?sslmode=disable"
	var out, errOut bytes.Buffer
	code := runMigrationsCtl([]string{"status"}, envOf(map[string]string{"DATABASE_URL": dsn}), &out, &errOut)
	combined := out.String() + errOut.String()
	// Mutation: surface the driver error, which can echo host, user or password.
	require.Equal(t, 1, code)
	require.Equal(t, unavailableLine, errOut.String())
	require.Empty(t, out.String())
	for _, frag := range []string{sentinelDSNMark, "admin", "127.0.0.1", "app"} {
		require.NotContains(t, combined, frag)
	}
}

func TestMigrationsMode_Parse(t *testing.T) {
	ok := map[string]bool{
		"":       false,
		"apply":  false,
		"verify": true,
	}
	for in, want := range ok {
		got, err := parseMigrationsMode(in)
		// Mutation: map "" to verify, or "verify" to apply.
		require.NoError(t, err, "input %q", in)
		require.Equal(t, want, got, "input %q", in)
	}

	for _, in := range []string{"Verify", "APPLY", "skip", " verify", "verify ", "off"} {
		_, err := parseMigrationsMode(in)
		// Mutation: case-fold or trim the value, or default unknown values to apply.
		require.EqualError(t, err, "invalid MIGRATIONS_MODE: want empty, apply or verify", "input %q", in)
	}
}

func TestMigrationsMode_VerifyNeverApplies(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name        string
		verifyOnly  bool
		applyErr    error
		verifyErr   error
		wantApply   int
		wantVerify  int
		wantErrIs   error
		wantNoError bool
	}{
		{"verify ok", true, nil, nil, 0, 1, nil, true},
		{"verify refuses", true, nil, boom, 0, 1, boom, false},
		{"apply ok", false, nil, nil, 1, 0, nil, true},
		{"apply fails", false, boom, nil, 1, 0, boom, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyCalls, verifyCalls := 0, 0
			err := runStartupMigrations(tc.verifyOnly,
				func() error { applyCalls++; return tc.applyErr },
				func() error { verifyCalls++; return tc.verifyErr })
			// Mutation: verify mode also calls apply (before, after, or on verify failure), or apply mode calls verify.
			require.Equal(t, tc.wantApply, applyCalls)
			require.Equal(t, tc.wantVerify, verifyCalls)
			if tc.wantNoError {
				require.NoError(t, err)
			} else {
				// Mutation: swallow the callee's error.
				require.ErrorIs(t, err, tc.wantErrIs)
			}
		})
	}
}

// TestServePathRefusesInvalidModeBeforeOpeningDatabase drives the serve path
// itself. DATABASE_URL names a socket directory that does not exist, so a serve
// path that reached database.New before parsing MIGRATIONS_MODE would return
// the connection error instead of the mode error; the control case proves the
// fixture fails there when the mode is valid.
func TestServePathRefusesInvalidModeBeforeOpeningDatabase(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("DATABASE_URL", "postgres:///concord?host=/nonexistent-concord-test-socket&sslmode=disable")

	t.Run("invalid mode stops before the database", func(t *testing.T) {
		t.Setenv("MIGRATIONS_MODE", "Verify")
		// Mutation: parse MIGRATIONS_MODE after database.New, or default an unknown value to apply.
		require.EqualError(t, runControlPlane(), "configure migrations: invalid MIGRATIONS_MODE: want empty, apply or verify")
	})

	t.Run("control: a valid mode reaches the unreachable database", func(t *testing.T) {
		t.Setenv("MIGRATIONS_MODE", "verify")
		require.ErrorContains(t, runControlPlane(), "connect to database:")
	})
}
