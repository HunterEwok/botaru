// SPDX-License-Identifier: AGPL-3.0-only

// Package pgtest gives tests a fresh, empty PostgreSQL database. With BOTARU_TEST_DSN it creates
// a database on that server; otherwise it starts a private server with initdb from PATH or from
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
)

// Main runs the tests of a package and stops the private server afterwards; call it from
// TestMain.
func Main(m interface{ Run() int }) {
	code := m.Run()
	if stopFn != nil {
		stopFn()
	}
	os.Exit(code)
}

// DSN returns the connection string of a new empty database for this test; the database is
// dropped when the test ends.
func DSN(t testing.TB) string {
	t.Helper()
	once.Do(setup)
	if setupErr != nil {
		if os.Getenv("BOTARU_TEST_PG_REQUIRED") == "1" {
			t.Fatalf("pgtest: %v", setupErr)
		}
		t.Skipf("pgtest: no PostgreSQL: %v", setupErr)
	}
	name := "botaru_" + strings.ToLower(strings.TrimPrefix(ids.New("x"), "x_"))
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, serverDSN)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
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
	opts := fmt.Sprintf("-p %d -k %s -c listen_addresses=127.0.0.1 -c fsync=off -c synchronous_commit=off -c full_page_writes=off -c max_connections=200", port, dir)
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
