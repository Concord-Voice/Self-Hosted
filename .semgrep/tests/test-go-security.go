package tests

// Test fixture for concord-go-security rules
// This file should trigger Semgrep findings — it is NOT production code.

import (
	"database/sql"
	"fmt"
	"net/http"
)

func unsafeQuery(db *sql.DB, userInput string) {
	// ruleid: concord-go-sql-sprintf
	query := fmt.Sprintf("SELECT * FROM users WHERE name = '%s'", userInput)
	db.Query(query)
}

func noTLS() {
	// ruleid: concord-go-no-tls
	http.ListenAndServe(":8080", nil)
}
