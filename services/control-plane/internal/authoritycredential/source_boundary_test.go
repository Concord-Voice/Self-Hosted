package authoritycredential

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionImportsRemainDormant(t *testing.T) {
	packagePath := "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/" + "authority" + "credential"
	root := filepath.Clean(filepath.Join("..", ".."))
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/authoritycredential/") || strings.HasPrefix(filepath.ToSlash(rel), "cmd/credential-authority-vectors/") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			if strings.Trim(imported.Path.Value, "\"") == packagePath {
				return &unexpectedImport{path: path}
			}
		}
		return nil
	})
	require.NoError(t, err)
}

type unexpectedImport struct{ path string }

func (e *unexpectedImport) Error() string {
	return "dormant authority credential codec imported by " + e.path
}
