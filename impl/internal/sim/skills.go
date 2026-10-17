// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
)

// skillFolders reads the skill folders of examples/skills as apply carries them (SKL-1): the
// files of each folder by slash path, with their bytes and mode. The reference bot ops-reader
// pins one of them, and its tasks wait with skill_missing without it.
func skillFolders(root string) (map[string][]map[string]any, error) {
	dir := filepath.Join(root, "examples", "skills")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]map[string]any{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		base := filepath.Join(dir, e.Name())
		var files []map[string]any
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files = append(files, map[string]any{"path": filepath.ToSlash(rel), "data": base64.StdEncoding.EncodeToString(b), "mode": "file"})
			return nil
		})
		if err != nil {
			return nil, err
		}
		out[e.Name()] = files
	}
	return out, nil
}
