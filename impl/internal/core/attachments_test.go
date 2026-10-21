// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// A file a person attaches reaches data/describe (D-72, flow 12): the tool's file parameter
// lists only the task's attachments, a file of another task fails the schema, the call
// handout names the file, and under the call lease file.get hands out that file and no other
// (TEN-7, P3 item 6).
func TestAttachmentReachesTool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		root := filepath.Join("..", "..", "..")
		docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/tools/data/tool.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			ms = append(ms, d.Value)
		}
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "Role":
				s["tools"] = append(s["tools"].([]any), "data/describe")
			case "Group":
				s["data"] = []any{"summary", "internal"}
			}
		}
		return ms
	})
	alice := &OnBehalf{Identity: "telegram:1001", Chat: Chat{Kind: "direct", ExternalID: "1001"}}
	ctd := []byte("depth,temp\n1,4.2\n")
	up, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/csv"}, ctd)
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/csv"}, []byte("not attached\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello describe it", Attachments: []string{up.FileRef}}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	r, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || r.Result != "task" {
		t.Fatalf("submit %+v %v", r, err)
	}
	// The tool is tier 1: the task may use it.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"max_tier": 1}' WHERE id = $1`, r.Task); err != nil {
		t.Fatal(err)
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	h, _ := att.Payload["handles"].(map[string]any)["tools"].(string)
	l, err := b.c.P3ToolsList(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	var enum []any
	for _, x := range l.Tools {
		m := x.(map[string]any)
		if m["name"] == "data_describe" {
			enum, _ = m["inputSchema"].(map[string]any)["properties"].(map[string]any)["file"].(map[string]any)["enum"].([]any)
		}
	}
	if !slices.Equal(enum, []any{up.FileRef}) {
		t.Fatalf("the file enum %v, want only %s", enum, up.FileRef)
	}
	if res, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "data_describe", Arguments: map[string]any{"file": other.FileRef}}); err != nil || !res.IsError {
		t.Fatalf("a file the task does not have: %+v %v", res, err)
	}
	th := b.toolhost(t, "data")
	done := callAsync(b, h, mcp.CallParams{Name: "data_describe", Arguments: map[string]any{"file": up.FileRef}})
	item := b.claimTool(t, th)
	files, _ := item.Payload["files"].([]string)
	if !slices.Equal(files, []string{up.FileRef}) {
		t.Fatalf("the handout names %v", item.Payload["files"])
	}
	lh := header(t, item)
	got, err := b.c.FileGet(ctx, th, lh, FileGetRequest{FileRef: up.FileRef})
	if err != nil || string(got.Content) != string(ctd) {
		t.Fatalf("file.get of the attachment: %+v %v", got, err)
	}
	if _, err := b.c.FileGet(ctx, th, lh, FileGetRequest{FileRef: other.FileRef}); reasonOf(err) != "not_found" {
		t.Fatalf("file.get of a file the call does not name: %v", err)
	}
	out := map[string]any{"structuredContent": map[string]any{"stats": map[string]any{"records": 1, "depth_max_m": 1, "flagged_records": 0}, "header": ""}}
	if _, err := b.c.Complete(ctx, th, lh, CompleteRequest{Output: out}); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.err != nil || res.r.IsError {
		t.Fatalf("result %+v %v", res.r, res.err)
	}
}
