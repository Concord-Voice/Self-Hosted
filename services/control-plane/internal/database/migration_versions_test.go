package database_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// golang-migrate refuses two files with one version, so two changes that
// pick the same number fail only at deploy. Each version has exactly one up
// and one down file, both with one name. Gaps are allowed: a number can be
// held by a change that has not merged yet.
func TestMigrationVersionsAreUniqueAndPaired(t *testing.T) {
	entries, err := os.ReadDir("../../migrations")
	require.NoError(t, err)
	file := regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)
	type pair struct {
		name     string
		up, down int
	}
	versions := map[string]*pair{}
	for _, e := range entries {
		m := file.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		p := versions[m[1]]
		if p == nil {
			p = &pair{name: m[2]}
			versions[m[1]] = p
		}
		assert.Equal(t, p.name, m[2], "version %s has two names", m[1])
		if m[3] == "up" {
			p.up++
		} else {
			p.down++
		}
	}
	require.NotEmpty(t, versions)
	for v, p := range versions {
		assert.Equal(t, 1, p.up, "version %s up files", v)
		assert.Equal(t, 1, p.down, "version %s down files", v)
	}
}
