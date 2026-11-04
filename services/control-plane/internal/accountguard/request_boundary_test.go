package accountguard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This is a narrow syntactic ownership ratchet, not Decide purity or alias analysis.
// In particular, an Outcome-accepting wrapper must not silently apply just its request slice.
func requestOutcomeWrapper(text string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", text, 0)
	if err != nil {
		return true
	}
	aliases := map[string]bool{}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		if p == sourcePackage+"/decide" {
			name := "decide"
			if im.Name != nil {
				name = im.Name.Name
			}
			aliases[name] = true
		}
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		outcome := false
		ast.Inspect(fn.Type, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := s.X.(*ast.Ident); ok && aliases[id.Name] && s.Sel.Name == "Outcome" {
					outcome = true
				}
			}
			return true
		})
		applies := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "applyTx" {
				applies = true
			}
			return true
		})
		if outcome && applies {
			return true
		}
	}
	return false
}

func requestApplyReference(text string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", text, 0)
	if err != nil {
		return true
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "applyTx" {
			found = true
		}
		return true
	})
	return found
}

func TestRequestStoreProductionCallerRatchetControls(t *testing.T) {
	for _, c := range []struct {
		text string
		bad  bool
	}{
		{`package accountguard; func f(){store.applyTx(ctx,tx,u,now,p,o)}`, true},
		{`package accountguard; var escape=requestStore.applyTx`, true},
		{`package accountguard; func (requestStore) applyTx(){tx.ExecContext(ctx,q)}`, false},
		{`package accountguard; // store.applyTx(ctx,tx,u,now,p,o)
func f(){}`, false},
	} {
		if requestApplyReference(c.text) != c.bad {
			t.Fatal("unexpected dormant request caller classification")
		}
	}
}

func TestRequestStoreOutcomeWrapperRatchetControls(t *testing.T) {
	for _, c := range []struct {
		name, body string
		bad        bool
	}{
		{"discarded remainder", `func f(o d.Outcome){p,_,_:=partitionRequestEffects(o);store.applyTx(ctx,tx,u,now,p,opens)}`, true},
		{"escaped apply", `func f(o d.Outcome){run:=store.applyTx;_ = run}`, true},
		{"pure partition", `func f(o d.Outcome){partitionRequestEffects(o)}`, false},
		{"typed component", `func f(p requestEffectPlan){store.applyTx(ctx,tx,u,now,p,opens)}`, false},
		{"comment", `// store.applyTx(ctx,tx,u,now,p,opens)
func f(o d.Outcome){}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := `package accountguard; import d "` + sourcePackage + `/decide";` + c.body
			if requestOutcomeWrapper(text) != c.bad {
				t.Fatal("unexpected complete-Outcome wrapper classification")
			}
		})
	}
}

func TestRequestStoreHasNoCompleteOutcomeExecutionWrapper(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := root.Close(); e != nil {
			t.Error(e)
		}
	})
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, e := root.ReadFile(filepath.Base(name))
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(raw), "func partitionRequestEffects(") {
			seen = true
		}
		if requestOutcomeWrapper(string(raw)) {
			t.Errorf("%s: complete-Outcome execution wrapper violates request-only ownership", name)
		}
		if requestApplyReference(string(raw)) {
			t.Errorf("%s: PR3b must have no production request apply caller or escaped method", name)
		}
	}
	if !seen {
		t.Fatal("request partition owner must exist; empty source census is not evidence")
	}
}
