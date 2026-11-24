package database

import (
	"context"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
)

// TestWithPoolSessionParams checks the DSN the driver will actually use: each
// result is parsed back through lib/pq and must carry the run-time parameter
// it sends at connection startup.
func TestWithPoolSessionParams(t *testing.T) {
	cases := []struct {
		name, in, wantValue string
		unchanged           bool
	}{
		{name: "URL form", in: "postgres://u@db:5432/concord", wantValue: "60000"},
		{name: "URL form with a query string", in: "postgres://u@db:5432/concord?sslmode=disable&connect_timeout=5", wantValue: "60000"},
		{name: "postgresql scheme", in: "postgresql://u@db/concord?sslmode=require", wantValue: "60000"},
		{name: "key=value form", in: "host=db port=5432 user=u dbname=concord sslmode=disable", wantValue: "60000"},
		{name: "explicit URL value kept", in: "postgres://u@db/concord?idle_in_transaction_session_timeout=5000", wantValue: "5000", unchanged: true},
		{name: "explicit key=value value kept", in: "host=db dbname=concord idle_in_transaction_session_timeout=0", wantValue: "0", unchanged: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := withPoolSessionParams(tc.in)
			require.NoError(t, err)
			if tc.unchanged {
				assert.Equal(t, tc.in, out)
			}
			cfg, err := pq.NewConfig(out)
			require.NoError(t, err)
			assert.Equal(t, tc.wantValue, cfg.Runtime[poolIdleInTxParam])
			if tc.name == "URL form with a query string" {
				assert.Equal(t, "disable", string(cfg.SSLMode), "existing parameters survive")
			}
		})
	}

	t.Run("value inside options is kept", func(t *testing.T) {
		in := "host=db dbname=concord options='-c idle_in_transaction_session_timeout=1000'"
		out, err := withPoolSessionParams(in)
		require.NoError(t, err)
		assert.Equal(t, in, out)
	})

	t.Run("a malformed URL fails without echoing it", func(t *testing.T) {
		marker := "s3cret-marker"
		_, err := withPoolSessionParams("postgres://u:" + marker + "@db:notaport/concord")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), marker)
	})
}

// lib/pq lets a backslash escape whitespace inside an unquoted key=value
// value, so a DSN can end in an escaped space. Trimming it would leave the
// backslash to escape the separator before the appended parameter, folding
// that parameter into the value and leaving the timeout unset.
func TestWithPoolSessionParams_EscapedTrailingSpace(t *testing.T) {
	in := `host=db dbname=concord user=u password=test-pass\ `
	orig, err := pq.NewConfig(in)
	require.NoError(t, err)
	require.Equal(t, "test-pass ", orig.Password, "fixture check: the DSN's password ends in a space")

	out, err := withPoolSessionParams(in)
	require.NoError(t, err)
	cfg, err := pq.NewConfig(out)
	require.NoError(t, err)
	assert.Equal(t, "test-pass ", cfg.Password, "the password is unchanged")
	assert.Equal(t, poolIdleInTxValue, cfg.Runtime[poolIdleInTxParam], "the timeout is set")
}

// TestNew_SetsPoolIdleInTransactionTimeout is the consumer side: a session
// opened through New reports the bound the server actually applied.
func TestNew_SetsPoolIdleInTransactionTimeout(t *testing.T) {
	db, err := New(dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	var got string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&got))
	assert.Equal(t, "1min", got)
}
