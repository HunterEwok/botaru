// SPDX-License-Identifier: AGPL-3.0-only

package httpbound

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A client that sends its body too slowly is cut off without any credential, while a handler
// that keeps running after it read the body (a long poll) is not.
func TestBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "slow", http.StatusRequestTimeout)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
		_, _ = io.WriteString(w, "done")
	})
	s := httptest.NewUnstartedServer(nil)
	s.Config = Server("", h, 200*time.Millisecond, time.Second)
	s.Start()
	defer s.Close()

	resp, err := http.Post(s.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "done" {
		t.Fatalf("a long handler after the body: %q", b)
	}

	c, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n{")
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	if time.Since(start) > 2*time.Second || line == "" {
		t.Fatalf("a slow body held the connection: %q after %v", line, time.Since(start))
	}
}
