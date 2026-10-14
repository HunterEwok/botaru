// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/api"
	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/texts"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var update = flag.Bool("update", false, "rewrite testdata/passing.txt from this run")

// claim is what the reference core is checked against while v0.1 is being built: every
// profile of v0.1, so the table shows the whole way to the claim.
var claim = Claim{Version: "v0.1", Profiles: []string{
	"core", "insights", "governance", "manager", "messaging", "campaign", "monitor", "knowledge", "ingress", "connections", "debug", "sandbox",
}}

// inProcess is a bench of a fresh database and a core with its P1, P2 and test-control
// listeners in this process; the runner reaches it only over HTTP.
type inProcess struct {
	t    *testing.T
	root string
	sp   *spec.Spec
	tx   *texts.Catalog
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (b *inProcess) Fresh(ctx context.Context) (*Endpoints, func(), error) {
	st, err := store.Open(ctx, pgtest.DSN(b.t))
	if err != nil {
		return nil, nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, nil, err
	}
	c, err := core.New(ctx, core.Config{TokenKey: []byte("conform-key-conform-key-conform-key"), TestBench: true, Operator: "the test bench operator",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, b.sp, b.tx)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	ep := &Endpoints{Operators: map[string]string{}, TestKey: "test-" + ids.Secret(16)}
	var creds []api.OperatorCred
	for _, tenant := range []string{"demo", "demo2", "farm"} {
		tok := "op-" + ids.Secret(16)
		ep.Operators[tenant] = tok
		creds = append(creds, api.OperatorCred{TokenSHA256: sha(tok), Principal: ids.PrincipalID(), Tenant: tenant})
	}
	srv, err := api.New(c, api.Config{Operators: creds, TestKeySHA256: sha(ep.TestKey)}, nil)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	p1 := httptest.NewServer(srv.P1Handler())
	p2 := httptest.NewServer(srv.P2Handler())
	tc := httptest.NewServer(srv.TestHandler())
	ep.P1, ep.P2, ep.Test = p1.URL, p2.URL, tc.URL
	return ep, func() { p1.Close(); p2.Close(); tc.Close(); st.Close() }, nil
}

// TestConformance runs every behaviour case of the claim on the reference core. It fails when
// the set of passing cases differs from testdata/passing.txt: a case that stopped passing is a
// regression, and a case that started to pass is recorded with -update, so the list is the
// record of what is done. BOTARU_CONFORM_OUT names a folder for the JUnit report and the
// coverage table.
func TestConformance(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(root, claim, &inProcess{t: t, root: root, sp: sp, tx: tx})
	if err != nil {
		t.Fatal(err)
	}
	cases, err := LoadCases(root)
	if err != nil {
		t.Fatal(err)
	}
	results := r.Run(context.Background(), cases)
	if dir := os.Getenv("BOTARU_CONFORM_OUT"); dir != "" {
		var junit, cov bytes.Buffer
		if err := WriteJUnit(&junit, results); err != nil {
			t.Fatal(err)
		}
		if err := WriteCoverage(&cov, claim, Coverage(r.Rules, claim, results), results); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "junit.xml"), junit.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "coverage.md"), cov.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := Passing(results)
	path := filepath.Join("testdata", "passing.txt")
	if *update {
		head := "# Behaviour cases that pass on the reference core (TestConformance); rewrite with -update.\n"
		if err := os.WriteFile(path, []byte(head+strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			want[l] = true
		}
	}
	byID := map[string]Result{}
	for _, res := range results {
		byID[res.Case.ID] = res
	}
	for id := range want {
		if res := byID[id]; res.Status != Passed {
			t.Errorf("%s no longer passes: %s %s", id, res.Status, res.Detail)
		}
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("%s passes now; record it with go test ./internal/conform -run TestConformance -update", id)
		}
	}
}

// TestTestControlPort checks the port itself: only the test bench key opens it, the clock moves
// only forward and a loop this build lacks is refused as not implemented.
func TestTestControlPort(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ep, stop, err := (&inProcess{t: t, root: root, sp: sp, tx: tx}).Fresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	r := &Runner{Client: http.DefaultClient}
	x := &exec{r: r, ep: ep}
	post := func(op, key string, body map[string]any) (int, map[string]any) {
		t.Helper()
		status, resp, err := x.post(ctx, ep.Test+"/v1/test/"+op, key, body)
		if err != nil {
			t.Fatal(err)
		}
		return status, resp
	}
	if status, _ := post("clock.advance", ep.Operators["demo"], map[string]any{"op": "clock.advance", "by_s": 60}); status != 401 {
		t.Fatalf("an operator credential opened the port: %d", status)
	}
	status, resp := post("clock.advance", ep.TestKey, map[string]any{"op": "clock.advance", "by_s": 3600})
	if status != 200 {
		t.Fatalf("clock.advance: %d %v", status, resp)
	}
	t1, err := time.Parse(time.RFC3339Nano, str(resp["now"]))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(t1); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("the store clock moved by %v, not an hour", d)
	}
	if status, resp := post("clock.set", ep.TestKey, map[string]any{"op": "clock.set", "at": "2020-01-01T00:00:00Z"}); status != 400 {
		t.Fatalf("the clock went back: %d %v", status, resp)
	}
	if status, resp := post("loop.tick", ep.TestKey, map[string]any{"loop": "reaper", "copies": 2}); status != 200 {
		t.Fatalf("loop.tick reaper: %d %v", status, resp)
	}
	status, resp = post("loop.tick", ep.TestKey, map[string]any{"loop": "schedule"})
	if o := problemOutcome(status, resp); o.Unimplemented == "" {
		t.Fatalf("a missing loop is not reported as unimplemented: %d %v", status, resp)
	}
	status, resp = post("store.snapshot", ep.TestKey, map[string]any{"label": "a"})
	if o := problemOutcome(status, resp); o.Unimplemented == "" {
		t.Fatalf("a missing operation is not reported as unimplemented: %d %v", status, resp)
	}
}
