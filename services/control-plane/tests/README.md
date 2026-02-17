# Control Plane Tests

This directory contains tests for the Concord Control Plane service.

## Structure

```
tests/
├── unit/         # Unit tests for individual functions/methods
├── integration/  # Integration tests for API endpoints
└── fixtures/     # Test data and fixtures
```

## Running Tests

### All Tests
```bash
go test -v ./...
```

### Unit Tests Only
```bash
go test -v ./tests/unit/...
```

### Integration Tests
```bash
# Start test database first
docker-compose up -d postgres redis

# Run integration tests
go test -v ./tests/integration/...
```

### With Coverage
```bash
go test -v -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

## Writing Tests

### Unit Test Example
```go
package unit

import (
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestSomeFunction(t *testing.T) {
    result := SomeFunction()
    assert.Equal(t, expected, result)
}
```

### Integration Test Example
```go
package integration

import (
    "testing"
    "net/http/httptest"
)

func TestAPIEndpoint(t *testing.T) {
    // Setup test server
    router := setupTestRouter()
    req := httptest.NewRequest("GET", "/api/v1/endpoint", nil)
    w := httptest.NewRecorder()

    // Execute
    router.ServeHTTP(w, req)

    // Assert
    assert.Equal(t, http.StatusOK, w.Code)
}
```

## Test Fixtures

Place test data in `fixtures/` directory:
- `fixtures/users.json` - Sample user data
- `fixtures/servers.json` - Sample server data
- `fixtures/messages.json` - Sample message data

## CI/CD

Tests run automatically on:
- Pull requests to main/develop
- Pushes to main/develop

See `.github/workflows/ci.yml` for CI configuration.
