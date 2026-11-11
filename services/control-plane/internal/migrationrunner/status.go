package migrationrunner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/lib/pq"
)

// versionTable is golang-migrate's default version table, which Run creates
// in the connection's current schema.
const versionTable = "schema_migrations"

var errSchemaNotCurrent = errors.New("schema not current for this binary (verify mode)")

// Status is the recorded schema version set against the latest version a
// migration source carries. Version and Dirty are meaningful only when
// Initialised is true.
type Status struct {
	Initialised bool
	Version     uint
	Dirty       bool
	Latest      uint
}

// Current reports whether the database is clean at exactly the latest version.
func (s Status) Current() bool {
	return s.Initialised && !s.Dirty && s.Version == s.Latest
}

// Line renders s as the one-line `migrations status` output. A dirty database
// always reads dirty, whatever its version is relative to the source.
func (s Status) Line() string {
	db := strconv.FormatUint(uint64(s.Version), 10)
	latest := strconv.FormatUint(uint64(s.Latest), 10)
	switch {
	case !s.Initialised:
		return "uninitialised " + latest
	case s.Dirty:
		return "dirty " + db
	case s.Version == s.Latest:
		return "current " + db
	case s.Version < s.Latest:
		return "pending " + db + " " + latest
	default:
		return "ahead " + db + " " + latest
	}
}

// ReadStatus reads the recorded version from db and the latest version from
// sourceURL. It runs one read-only transaction and never builds the migrate
// driver, so it takes no advisory lock and never creates the version table.
// Only file:// is registered, and sourceURL must be a trusted constant.
func ReadStatus(ctx context.Context, db *sql.DB, sourceURL string) (Status, error) {
	st, err := readRecordedVersion(ctx, db)
	if err != nil {
		return Status{}, err
	}
	latest, err := latestSourceVersion(sourceURL)
	if err != nil {
		return Status{}, err
	}
	st.Latest = latest
	return st, nil
}

// Verify returns nil only when ReadStatus reports the schema current. It
// writes nothing, so an uninitialised, pending, ahead or dirty schema stays as
// it is.
func Verify(ctx context.Context, db *sql.DB, sourceURL string) error {
	st, err := ReadStatus(ctx, db, sourceURL)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	if !st.Current() {
		return errSchemaNotCurrent
	}
	return nil
}

func readRecordedVersion(ctx context.Context, db *sql.DB) (st Status, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Status{}, fmt.Errorf("begin read-only migration status transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			st = Status{}
			err = errors.Join(err, fmt.Errorf("roll back migration status transaction: %w", rollbackErr))
		}
	}()

	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
		return Status{}, fmt.Errorf("set migration status statement timeout: %w", err)
	}

	var schema sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return Status{}, fmt.Errorf("read current schema: %w", err)
	}
	if !schema.Valid {
		return Status{}, errors.New("read current schema: no current schema")
	}
	qualified := pq.QuoteIdentifier(schema.String) + "." + pq.QuoteIdentifier(versionTable)

	var table sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)`, qualified).Scan(&table); err != nil {
		return Status{}, fmt.Errorf("look up migration version table: %w", err)
	}
	if !table.Valid {
		return Status{}, nil
	}

	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- identifiers are quoted with pq.QuoteIdentifier
	versionSQL := `SELECT version, dirty FROM ` + qualified // #nosec G202 -- both identifiers are quoted with pq.QuoteIdentifier
	rows, err := tx.QueryContext(ctx, versionSQL)           // NOSONAR -- go:S2077: both identifiers are quoted with pq.QuoteIdentifier; a table name cannot be a bind parameter
	if err != nil {
		return Status{}, fmt.Errorf("read migration version: %w", err)
	}
	// Closed and checked below on the success path.
	defer func() { _ = rows.Close() }()

	found := false
	for rows.Next() {
		if found {
			return Status{}, errors.New("read migration version: version table holds more than one row")
		}
		var version int64
		if err := rows.Scan(&version, &st.Dirty); err != nil {
			return Status{}, fmt.Errorf("scan migration version: %w", err)
		}
		// golang-migrate records a failed migration down past the first
		// version as a dirty -1, which no Status can represent.
		if version < 0 {
			return Status{}, fmt.Errorf("read migration version: recorded version %d is negative", version)
		}
		st.Version = uint(version)
		found = true
	}
	if err := rows.Err(); err != nil {
		return Status{}, fmt.Errorf("iterate migration version: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Status{}, fmt.Errorf("close migration version rows: %w", err)
	}
	if !found {
		return Status{}, nil
	}
	st.Initialised = true
	return st, nil
}

// latestSourceVersion walks sourceURL from First through Next to its last
// version. An empty or unreadable source is an error, never version 0.
func latestSourceVersion(sourceURL string) (latest uint, err error) {
	src, err := source.Open(sourceURL)
	if err != nil {
		return 0, fmt.Errorf("open migration source: %w", err)
	}
	defer func() {
		if closeErr := src.Close(); closeErr != nil {
			latest = 0
			err = errors.Join(err, fmt.Errorf("close migration source: %w", closeErr))
		}
	}()

	version, err := src.First()
	if errors.Is(err, os.ErrNotExist) {
		return 0, errors.New("read migration source: source holds no migrations")
	}
	if err != nil {
		return 0, fmt.Errorf("read first migration: %w", err)
	}
	for {
		next, err := src.Next(version)
		if errors.Is(err, os.ErrNotExist) {
			return version, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read migration after version %d: %w", version, err)
		}
		version = next
	}
}
