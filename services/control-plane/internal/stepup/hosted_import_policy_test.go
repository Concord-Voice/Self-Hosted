package stepup

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStepupSourceAvoidsHostedImportScrubMarkers(t *testing.T) {
	source, err := os.ReadFile("stepup.go") // #nosec G304 -- fixed sibling source file.
	require.NoError(t, err)

	scrubMarker := "[" + "internal]"
	require.NotContains(t, string(source), scrubMarker,
		"the hosted importer rejects this marker in imported service sources")
}
