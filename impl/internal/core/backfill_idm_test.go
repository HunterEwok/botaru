// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// coreKeyExtras are the core keys of this build beyond the closed list of IDM-2, each named in
// the implementation notes: system:postmortem (named by MGR-53), system:grant (the action task
// of a routine service grant), system:directive and system:kb.ingest.
var coreKeyExtras = []string{"postmortem", "grant", "directive", "kb.ingest"}

// coreKeyKinds reads the kinds system:<kind>:... of the closed list from the text of IDM-2 in
// conformance/rules.md and adds the extras.
func coreKeyKinds(t *testing.T) []string {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "conformance/rules.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "### IDM-2.")
	end := strings.Index(text[start:], "Checked by:")
	if start < 0 || end < 0 {
		t.Fatal("no text of IDM-2")
	}
	var kinds []string
	for _, m := range regexp.MustCompile("`system:([a-z_.]+):").FindAllStringSubmatch(text[start:start+end], -1) {
		if !slices.Contains(kinds, m[1]) {
			kinds = append(kinds, m[1])
		}
	}
	if len(kinds) < 20 {
		t.Fatalf("the list of IDM-2 has %d kinds", len(kinds))
	}
	return append(kinds, coreKeyExtras...)
}

// keyPrefix is the leading string literal of a key expression: a literal, a concatenation that
// starts with one, a fmt.Sprintf format, or a local variable assigned one of them.
func keyPrefix(e ast.Expr, vars map[string][]ast.Expr, depth int) []string {
	if depth > 4 {
		return nil
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if s, err := strconv.Unquote(x.Value); err == nil {
			return []string{s}
		}
	case *ast.BinaryExpr:
		return keyPrefix(x.X, vars, depth+1)
	case *ast.ParenExpr:
		return keyPrefix(x.X, vars, depth+1)
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(x.Args) > 0 {
			return keyPrefix(x.Args[0], vars, depth+1)
		}
	case *ast.Ident:
		var out []string
		for _, v := range vars[x.Name] {
			out = append(out, keyPrefix(v, vars, depth+1)...)
		}
		return out
	}
	return nil
}

// Tasks the core creates itself get a key only from the closed list, every one of them starts
// with system:, which no sender may use, and a double firing of one trigger gives one task
// (IDM-2, blackbox:core/system-key-list).
func TestBackfillCoreKeys(t *testing.T) {
	kinds := coreKeyKinds(t)
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			vars := map[string][]ast.Expr{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok && as.Tok != token.ADD_ASSIGN && len(as.Lhs) == len(as.Rhs) {
					for i, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							vars[id.Name] = append(vars[id.Name], as.Rhs[i])
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				kv, ok := n.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "IdempotencyKey" {
					return true
				}
				for _, p := range keyPrefix(kv.Value, vars, 0) {
					found++
					pos := fset.Position(kv.Pos())
					// IDM-2: a core key starts with system: and names a kind of the list.
					kind, _, _ := strings.Cut(strings.TrimPrefix(p, "system:"), ":")
					if !strings.HasPrefix(p, "system:") || !slices.Contains(kinds, kind) {
						t.Errorf("%s:%d: the core key %q is not of the closed list", pos.Filename, pos.Line, p)
					}
				}
				return true
			})
		}
	}
	if found < 15 {
		t.Fatalf("only %d core keys found", found)
	}

	// IDM-2: a sender's key never starts with system:.
	b := newBench(t)
	if err := b.c.spec.Validate("enqueue-request", map[string]any{"body": greet("x"), "idempotency_key": "system:schedule:x:1"}); err == nil {
		t.Fatal("a sender's system: key passed the schema")
	}
	// IDM-2: a double firing of one trigger gives one task: the same channel update.
	first, again := b.hello(t, "1"), b.hello(t, "1")
	if first != again || b.count(t, `SELECT count(*) FROM tasks WHERE idempotency_key = 'system:activity:telegram:1'`) != 1 {
		t.Fatalf("a repeated update: %s %s", first, again)
	}
	// IDM-2: so does the incident loop running twice over one firing alarm.
	m := newManagerBench(t)
	m.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	m.diskAlarm(t, true, 3)
	m.advance(t, 6*60)
	m.loop(t, "incident")
	m.loop(t, "incident")
	if n := m.count(t, `SELECT count(*) FROM tasks WHERE type = $1 AND idempotency_key LIKE 'system:incident:%' GROUP BY record->>'incident' ORDER BY 1 DESC LIMIT 1`, incidentRespondType); n != 1 {
		t.Fatalf("seat tasks of one incident: %d", n)
	}
}
