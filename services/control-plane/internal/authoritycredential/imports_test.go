package authoritycredential

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionImportClosureIsPure(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "-json", ".")
	output, err := command.Output()
	require.NoError(t, err)
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	moduleInternal := "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/"
	for decoder.More() {
		var pkg struct{ ImportPath string }
		require.NoError(t, decoder.Decode(&pkg))
		if pkg.ImportPath == "database/sql" || strings.HasPrefix(pkg.ImportPath, moduleInternal) && pkg.ImportPath != moduleInternal+"authoritycontract" && pkg.ImportPath != moduleInternal+"authoritycredential" {
			t.Fatalf("authoritycredential must remain pure; unexpected dependency %q", pkg.ImportPath)
		}
	}
}
