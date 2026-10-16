// SPDX-License-Identifier: AGPL-3.0-only

package p1client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// FilePut puts a file through P1 file.put: a multipart body with the meta part and the content
// part, without a lease; headers carries Botaru-On-Behalf and Botaru-Chat for a channel's file.
// It returns the file_ref.
func (c *Client) FilePut(ctx context.Context, headers map[string]string, meta map[string]any, content []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="meta"`)
	h.Set("Content-Type", "application/json")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(raw); err != nil {
		return "", err
	}
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="content"; filename="content"`)
	h.Set("Content-Type", "application/octet-stream")
	if part, err = mw.CreatePart(h); err != nil {
		return "", err
	}
	if _, err := part.Write(content); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/p1/file.put", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.Token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(b, e) != nil || e.Reason == "" {
			e.Reason = "not_ready"
		}
		return "", e
	}
	var out struct {
		FileRef string `json:"file_ref"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	return out.FileRef, nil
}
