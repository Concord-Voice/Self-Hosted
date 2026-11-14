package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Every refresh-cookie writer must be classified when a new login route is added.
func TestSSOCookieRouteInventory(t *testing.T) {
	workdir, err := os.Getwd()
	require.NoError(t, err)
	internal := filepath.Clean(filepath.Join(workdir, ".."))
	writers := make(map[string]int)
	err = filepath.WalkDir(internal, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(internal, path)
		if relErr != nil {
			return relErr
		}
		for _, declaration := range file.Decls {
			fn, isFunc := declaration.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				switch target := call.Fun.(type) {
				case *ast.Ident:
					if target.Name == "SetRefreshCookie" {
						writers[filepath.ToSlash(rel)+":"+fn.Name.Name]++
					}
				case *ast.SelectorExpr:
					if target.Sel.Name == "SetRefreshCookie" {
						writers[filepath.ToSlash(rel)+":"+fn.Name.Name]++
					}
				}
				return true
			})
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, map[string]int{
		"auth/handlers.go:issueSessionTokens":       1, // register/confirm
		"auth/handlers.go:CompleteLogin":            1, // login and mfa/verify
		"auth/handlers.go:rotateAndRespond":         1, // refresh exemption
		"auth/handlers.go:handleGracePeriodRefresh": 1, // refresh exemption
		"auth/handlers.go:Logout":                   1, // logout exemption, clear only
		"auth/sso_link.go:CompleteSSOLink":            1, // mfa/verify link completion
		"oauth/handler.go:respondExistingSSO":       2, // provider/session
		"oauth/handler.go:CompleteRegistration":     1, // provider/complete-registration
		"oauth/handler.go:CompleteLink":             1, // provider/complete-link
	}, writers)
}

func TestLoginCSRFCookieExemptionAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		write      func(*gin.Context)
		cookieName string
		sameSite   http.SameSite
	}{
		{"refresh set", "/api/v1/auth/refresh", func(c *gin.Context) { auth.SetRefreshCookie(c, "token", 3600) }, "refresh_token", http.SameSiteLaxMode},
		{"logout clear", "/api/v1/auth/logout", func(c *gin.Context) { auth.SetRefreshCookie(c, "", -1) }, "refresh_token", http.SameSiteLaxMode},
		{"admin set", "/admin/api/v1/auth/webauthn", func(c *gin.Context) { admin.SetAdminSessionCookie(c, "sid") }, "__Host-cv_admin_sid", http.SameSiteStrictMode},
		{"admin clear", "/admin/api/v1/auth/logout", admin.ClearAdminSessionCookie, "__Host-cv_admin_sid", http.SameSiteStrictMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			c.Request.Header.Set("X-Forwarded-Proto", "https")
			tc.write(c)
			cookies := w.Result().Cookies()
			require.Len(t, cookies, 1)
			cookie := cookies[0]
			require.Equal(t, tc.cookieName, cookie.Name)
			require.Equal(t, "/", cookie.Path)
			require.True(t, cookie.HttpOnly)
			require.True(t, cookie.Secure)
			require.Equal(t, tc.sameSite, cookie.SameSite)
			if strings.Contains(tc.name, "clear") {
				require.Equal(t, "", cookie.Value)
				require.Equal(t, -1, cookie.MaxAge)
			}
		})
	}
}
