package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
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
	ok := map[string]migrationsMode{
		"":       migrationsModeApply,
		"apply":  migrationsModeApply,
		"verify": migrationsModeVerify,
	}
	for in, want := range ok {
		got, err := parseMigrationsMode(in)
		// Mutation: map "" to verify, or "verify" to apply.
		require.NoError(t, err, "input %q", in)
		require.Equal(t, want, got, "input %q", in)
	}
	require.NotEqual(t, migrationsModeApply, migrationsModeVerify)

	for _, in := range []string{"Verify", "APPLY", "skip", " verify", "verify ", "off"} {
		_, err := parseMigrationsMode(in)
		// Mutation: case-fold or trim the value, or default unknown values to apply.
		require.ErrorContains(t, err, "invalid MIGRATIONS_MODE", "input %q", in)
	}
}

func TestMigrationsMode_VerifyNeverApplies(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name        string
		mode        migrationsMode
		applyErr    error
		verifyErr   error
		wantApply   int
		wantVerify  int
		wantErrIs   error
		wantNoError bool
	}{
		{"verify ok", migrationsModeVerify, nil, nil, 0, 1, nil, true},
		{"verify refuses", migrationsModeVerify, nil, boom, 0, 1, boom, false},
		{"apply ok", migrationsModeApply, nil, nil, 1, 0, nil, true},
		{"apply fails", migrationsModeApply, boom, nil, 1, 0, boom, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyCalls, verifyCalls := 0, 0
			err := runStartupMigrations(tc.mode,
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

func TestServePathParsesModeBeforeOpeningDatabase(t *testing.T) {
	source, err := os.ReadFile("main.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	text := string(source)
	parse := strings.Index(text, "parseMigrationsMode(")
	open := strings.Index(text, "database.New(cfg.DatabaseURL)")
	startup := strings.Index(text, "runStartupMigrations(")
	// Mutation: parse the mode after database.New, or revert to the direct RunMigrations call.
	require.GreaterOrEqual(t, parse, 0, "serve path must parse MIGRATIONS_MODE")
	require.GreaterOrEqual(t, open, 0)
	require.Less(t, parse, open, "invalid MIGRATIONS_MODE must fail before any database connection")
	require.GreaterOrEqual(t, startup, 0, "serve path must route migrations through runStartupMigrations")
}
