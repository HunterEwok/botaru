// SPDX-License-Identifier: AGPL-3.0-only

// Package pgtest gives tests a fresh PostgreSQL database: an empty one (DSN) or a copy of a
// template prepared once per test process (TemplateDSN). With BOTARU_TEST_DSN it creates the
// databases on that server; otherwise it starts a private server with initdb from PATH or from
// /usr/lib/postgresql/*/bin for the test binary. Without PostgreSQL the test is skipped, unless
// BOTARU_TEST_PG_REQUIRED=1 (set in CI), when it fails.
package pgtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

var (
	once      sync.Once
	serverDSN string
	setupErr  error
	stopFn    func()

	templatesMu sync.Mutex
	templates   = map[string]*template{}
)

// template is a database prepared once per test process, which TemplateDSN copies.
type template struct {
	once sync.Once
	name string
	err  error
}

// Main runs the tests of a package, drops the templates of TemplateDSN and stops the private
// server afterwards; call it from TestMain.
func Main(m interface{ Run() int }) {
	code := m.Run()
	dropTemplates()
	if stopFn != nil {
		stopFn()
	}
	os.Exit(code)
}

// DSN returns the connection string of a new empty database for this test; the database is
// dropped when the test ends.
func DSN(t testing.TB) string {
	t.Helper()
	server(t)
	return create(t, "")
}

// TemplateDSN returns the connection string of a new database for this test, copied with
// CREATE DATABASE ... TEMPLATE from a database that prepare built once per test process; the
// copy is dropped when the test ends. key names what prepare puts in (for example a hash of the
// migrations) and is part of the template name, so a template of other contents is never
// taken. Each test process builds its own template under a name of its own, so the packages
// that go test runs at once as separate processes do not race on it; Main drops it at the end.
// prepare must close every connection it opened: PostgreSQL copies no database that has one.
func TemplateDSN(t testing.TB, key string, prepare func(ctx context.Context, dsn string) error) string {
	t.Helper()
	server(t)
	templatesMu.Lock()
	tp, ok := templates[key]
	if !ok {
		tp = &template{}
		templates[key] = tp
	}
	templatesMu.Unlock()
	tp.once.Do(func() { tp.name, tp.err = buildTemplate(key, prepare) })
	if tp.err != nil {
		t.Fatalf("pgtest: template: %v", tp.err)
	}
	return create(t, tp.name)
}

// server starts or finds the server once, and skips or fails the test without one.
func server(t testing.TB) {
	t.Helper()
	once.Do(setup)
	if setupErr != nil {
		if os.Getenv("BOTARU_TEST_PG_REQUIRED") == "1" {
			t.Fatalf("pgtest: %v", setupErr)
		}
		t.Skipf("pgtest: no PostgreSQL: %v", setupErr)
	}
}

func newName(prefix string) string {
	return prefix + strings.ToLower(strings.TrimPrefix(ids.New("x"), "x_"))
}

func buildTemplate(key string, prepare func(ctx context.Context, dsn string) error) (string, error) {
	ctx := context.Background()
	key = strings.ToLower(key)
	if len(key) > 16 {
		key = key[:16]
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "", fmt.Errorf("key %q: only letters and digits", key)
		}
	}
	name := newName("botaru_tpl_" + key + "_")
	conn, err := pgx.Connect(ctx, serverDSN)
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return "", err
	}
	if err := prepare(ctx, withDatabase(serverDSN, name)); err != nil {
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		return "", err
	}
	if _, err := conn.Exec(ctx, "ALTER DATABASE "+name+" WITH IS_TEMPLATE true"); err != nil {
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		return "", err
	}
	return name, nil
}

func dropTemplates() {
	templatesMu.Lock()
	defer templatesMu.Unlock()
	if serverDSN == "" {
		return
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, serverDSN)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	for key, tp := range templates {
		if tp.name != "" {
			_, _ = conn.Exec(ctx, "ALTER DATABASE "+tp.name+" WITH IS_TEMPLATE false")
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+tp.name+" WITH (FORCE)")
		}
		delete(templates, key)
	}
}

// create makes a new database for this test, empty or a copy of a template, and drops it when
// the test ends.
func create(t testing.TB, template string) string {
	t.Helper()
	name := newName("botaru_")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, serverDSN)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	defer conn.Close(ctx)
	q := "CREATE DATABASE " + name
	if template != "" {
		q += " TEMPLATE " + template
	}
	if _, err := conn.Exec(ctx, q); err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), serverDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return withDatabase(serverDSN, name)
}

func withDatabase(dsn, db string) string {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return dsn
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Host, cfg.Port, db)
}

func setup() {
	if dsn := os.Getenv("BOTARU_TEST_DSN"); dsn != "" {
		serverDSN = dsn
		return
	}
	if os.Geteuid() == 0 {
		setupErr = fmt.Errorf("initdb does not run as root; set BOTARU_TEST_DSN")
		return
	}
	bin, err := findBin()
	if err != nil {
		setupErr = err
		return
	}
	dir, err := os.MkdirTemp("", "botaru-pg-")
	if err != nil {
		setupErr = err
		return
	}
	data := filepath.Join(dir, "data")
	out, err := exec.Command(filepath.Join(bin, "initdb"), "-D", data, "-A", "trust", "-U", "postgres", "--no-sync").CombinedOutput()
	if err != nil {
		setupErr = fmt.Errorf("initdb: %v: %s", err, out)
		return
	}
	port, err := freePort()
	if err != nil {
		setupErr = err
		return
	}
	opts := fmt.Sprintf("-p %d -k %s -c listen_addresses=127.0.0.1 -c fsync=off -c synchronous_commit=off -c full_page_writes=off -c max_connections=400", port, dir)
	out, err = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-o", opts, "-l", filepath.Join(dir, "log"), "-w", "start").CombinedOutput()
	if err != nil {
		setupErr = fmt.Errorf("pg_ctl start: %v: %s", err, out)
		return
	}
	stopFn = func() {
		_ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run()
		_ = os.RemoveAll(dir)
	}
	serverDSN = fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := pgx.Connect(context.Background(), serverDSN)
		if err == nil {
			_ = c.Close(context.Background())
			return
		}
		if time.Now().After(deadline) {
			setupErr = err
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func findBin() (string, error) {
	if p, err := exec.LookPath("initdb"); err == nil {
		return filepath.Dir(p), nil
	}
	dirs, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	sort.Strings(dirs)
	for i := len(dirs) - 1; i >= 0; i-- {
		if _, err := os.Stat(filepath.Join(dirs[i], "initdb")); err == nil {
			return dirs[i], nil
		}
	}
	return "", fmt.Errorf("initdb not found")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
