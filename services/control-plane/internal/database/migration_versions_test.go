package database_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// golang-migrate refuses two files with one version and never applies a
// version below the one recorded, so two changes that pick the same number,
// or a number below one already merged, fail only at deploy. Each version
// from 1 to the highest has exactly one up and one down file with one name.
//
// releasedGaps lists versions that were never used. Released versions cannot
// be renumbered, so a gap stays; a new one is refused.
var releasedGaps = map[int]bool{192: true}

func TestMigrationVersionsAreUniqueAndContiguous(t *testing.T) {
	entries, err := os.ReadDir("../../migrations")
	require.NoError(t, err)
	file := regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)
	type pair struct {
		name     string
		up, down int
	}
	versions := map[int]*pair{}
	highest := 0
	for _, e := range entries {
		m := file.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		p := versions[v]
		if p == nil {
			p = &pair{name: m[2]}
			versions[v] = p
		}
		assert.Equal(t, p.name, m[2], "version %06d has two names", v)
		if m[3] == "up" {
			p.up++
		} else {
			p.down++
		}
		highest = max(highest, v)
	}
	require.Positive(t, highest)
	for v := 1; v <= highest; v++ {
		p := versions[v]
		if releasedGaps[v] {
			assert.Nil(t, p, "version %06d is a recorded gap; remove it from releasedGaps", v)
			continue
		}
		if !assert.NotNil(t, p, "version %06d is missing", v) {
			continue
		}
		assert.Equal(t, 1, p.up, "version %06d up files", v)
		assert.Equal(t, 1, p.down, "version %06d down files", v)
	}
}
