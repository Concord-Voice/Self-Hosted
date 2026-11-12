package stepup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyPasswordFactorSourceAvoidsHostedImportScrubMarkers(t *testing.T) {
	source, err := os.ReadFile("stepup.go") // #nosec G304 -- fixed sibling source file.
	require.NoError(t, err)

	scrubMarker := "[" + "internal]"
	require.NotContains(t, verifyPasswordFactorSource(t, source), scrubMarker,
		"the hosted importer rejects this marker in imported service sources")
}

func verifyPasswordFactorSource(t *testing.T, source []byte) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "stepup.go", source, parser.ParseComments)
	require.NoError(t, err)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "VerifyPasswordFactor" {
			continue
		}
		start := fset.PositionFor(fn.Pos(), false).Offset
		end := fset.PositionFor(fn.End(), false).Offset
		return string(source[start:end])
	}
	require.Fail(t, "VerifyPasswordFactor function not found")
	return ""
}
