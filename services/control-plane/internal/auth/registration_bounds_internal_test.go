package auth

import (
	"bytes"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegisterDecodedKeyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		label string
		limit int
	}{
		{"public key", 1024},
		{"wrapped private key", 32 * 1024},
		{"salt", 64},
	} {
		t.Run(tc.label, func(t *testing.T) {
			max := bytes.Repeat([]byte{0x42}, tc.limit)
			got, err := decodeBoundedBase64(base64.StdEncoding.EncodeToString(max), tc.label, tc.limit)
			require.NoError(t, err)
			require.Equal(t, max, got)
			_, err = decodeBoundedBase64(base64.StdEncoding.EncodeToString(append(max, 0x42)), tc.label, tc.limit)
			require.Error(t, err)
			_, err = decodeBoundedBase64("not-base64", tc.label, tc.limit)
			require.Error(t, err)
		})
	}
}

func TestRegisterBoundsAndUniquenessPrecedePasswordHash(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers.go", nil, 0)
	require.NoError(t, err)
	var register *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Register" {
			register = fn
			break
		}
	}
	require.NotNil(t, register)
	positions := map[string]token.Pos{}
	ast.Inspect(register.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if ok {
			positions[ident.Name] = call.Pos()
		}
		return true
	})
	for _, name := range []string{"bindRegisterRequest", "decodeE2EEKeys", "checkUsersUniqueness", "HashPassword"} {
		require.NotEqual(t, token.NoPos, positions[name], "%s must be called in Register", name)
	}
	require.Less(t, positions["bindRegisterRequest"], positions["decodeE2EEKeys"])
	require.Less(t, positions["decodeE2EEKeys"], positions["checkUsersUniqueness"])
	require.Less(t, positions["checkUsersUniqueness"], positions["HashPassword"])
}
