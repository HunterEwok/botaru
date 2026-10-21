// SPDX-License-Identifier: AGPL-3.0-only

package toolhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
)

// The attachments a call names are read with file.get under the call lease into a directory
// only the toolhost's user reads; a refused file or a ref that is not one fails the call, and
// copies older than any lease are swept at start (D-72, TEN-7).
func TestFetchFiles(t *testing.T) {
	var leases []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FileRef string `json:"file_ref"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		leases = append(leases, r.Header.Get("Botaru-Lease"))
		if r.URL.Path != "/v1/p1/file.get" || req.FileRef != "file:AAAA" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"reason":"not_found"}`))
			return
		}
		_, _ = w.Write([]byte("depth,temp\n1,4.2\n"))
	}))
	defer srv.Close()
	base := t.TempDir()
	stale := filepath.Join(base, filesPrefix+"old")
	fresh := filepath.Join(base, filesPrefix+"new")
	for _, d := range []string{stale, fresh} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleFiles)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Core: &p1client.Client{BaseURL: srv.URL, Token: "t"}, FilesDir: base})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a stale copy was kept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a recent copy was swept: %v", err)
	}

	files, dir, err := h.fetchFiles(t.Context(), "L1", []string{"file:AAAA"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(files["file:AAAA"])
	if err != nil || string(b) != "depth,temp\n1,4.2\n" || filepath.Dir(files["file:AAAA"]) != dir || leases[0] != "L1" {
		t.Fatalf("copy %q %v in %s, leases %v", b, err, dir, leases)
	}
	di, _ := os.Stat(dir)
	fi, _ := os.Stat(files["file:AAAA"])
	if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("modes %v %v", di.Mode().Perm(), fi.Mode().Perm())
	}
	h.removeFiles(dir)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the copies were kept: %v", err)
	}

	// A refused file after a read one: the call fails and its directory is handed back for removal.
	_, dir, err = h.fetchFiles(t.Context(), "L1", []string{"file:AAAA", "file:BBBB"})
	if p1client.Reason(err) != "not_found" || dir == "" {
		t.Fatalf("a refused file: %v, dir %q", err, dir)
	}
	h.removeFiles(dir)
	for _, ref := range []string{"file:../x", "note", "file:"} {
		if _, dir, err := h.fetchFiles(t.Context(), "L1", []string{ref}); err == nil {
			t.Fatalf("%q was read", ref)
		} else {
			h.removeFiles(dir)
		}
	}
	if files, dir, err := h.fetchFiles(t.Context(), "L1", nil); files != nil || dir != "" || err != nil {
		t.Fatal("a call without files made a directory")
	}
}
