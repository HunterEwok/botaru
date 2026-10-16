// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// filePut reads the multipart body of file.put: a meta part by p1-file-put-request and a
// content part with the bytes (spec/bindings/http/README.md, "Files").
func (s *Server) filePut(ctx context.Context, w http.ResponseWriter, r *http.Request, wk *core.Worker, schema string) {
	r.Body = http.MaxBytesReader(w, r.Body, core.MaxFileBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "file.put takes a multipart/form-data body"})
		return
	}
	var meta map[string]any
	var content []byte
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				// A body past the farm ceiling is refused whole (ART-4).
				Problem(w, &core.Problem{Reason: "file_too_large", Detail: "the file is too large"})
				return
			}
			Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the multipart body could not be read"})
			return
		}
		switch part.FormName() {
		case "meta":
			raw, err := io.ReadAll(io.LimitReader(part, 64<<10))
			if err != nil {
				Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the meta part could not be read"})
				return
			}
			var ok bool
			if meta, ok = s.object(w, raw, schema); !ok {
				return
			}
		case "content":
			if content, err = readContent(part); err != nil {
				Problem(w, &core.Problem{Reason: "file_too_large", Detail: "the file is too large"})
				return
			}
		}
	}
	if meta == nil || content == nil {
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "file.put needs the meta and content parts"})
		return
	}
	req, derr := decode[core.FilePutRequest](meta)
	if derr != nil {
		s.fail(w, derr)
		return
	}
	out, err := s.c.FilePut(ctx, wk, r.Header.Get("Botaru-Lease"), onBehalf(r), req, content)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, out)
}

func readContent(part *multipart.Part) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(part, core.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > core.MaxFileBytes {
		return nil, errors.New("too large")
	}
	return b, nil
}

// fileGet answers file.get with the bytes; the other fields go in headers.
func (s *Server) fileGet(ctx context.Context, w http.ResponseWriter, r *http.Request, wk *core.Worker, m map[string]any) {
	req, derr := decode[core.FileGetRequest](m)
	if derr != nil {
		s.fail(w, derr)
		return
	}
	out, err := s.c.FileGet(ctx, wk, r.Header.Get("Botaru-Lease"), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", out.MediaType)
	// The bytes are data: no client renders an active type of them in place.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(out.Content)), 10))
	w.Header().Set("Botaru-File-Ref", out.FileRef)
	w.Header().Set("Botaru-Data-Mark", out.DataMark)
	_, _ = w.Write(out.Content)
}
