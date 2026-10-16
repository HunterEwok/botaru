// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// skillNameRe is the folder name of a skill (the common name).
var skillNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// skillFolders are the skill folders of the farm git that go with the set (SKL-1, SKL-2): when
// a role of the set pins skills, every folder of the nearest skills/ above the role's file,
// with each file's bytes and its mode as the core lints it (file, executable or link). The core
// lints the folders and keeps its copy by their tree hash; farmctl only reads them.
func skillFolders(docs []manifest.Doc, ms []map[string]any) map[string][]map[string]any {
	var root string
	for i, d := range docs {
		spec, _ := ms[i]["spec"].(map[string]any)
		if d.Kind != "Role" || spec["skills"] == nil {
			continue
		}
		for dir := filepath.Dir(d.Source); ; dir = filepath.Dir(dir) {
			if st, err := os.Stat(filepath.Join(dir, "skills")); err == nil && st.IsDir() {
				root = filepath.Join(dir, "skills")
				break
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
		if root != "" {
			break
		}
	}
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := map[string][]map[string]any{}
	for _, e := range entries {
		if !e.IsDir() || !skillNameRe.MatchString(e.Name()) {
			continue
		}
		files, err := folderFiles(filepath.Join(root, e.Name()), true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skills/%s: %v\n", e.Name(), err)
			continue
		}
		out[e.Name()] = files
	}
	return out
}

// knowledgeFolders are the documents of the git corpora of the set's catalogs (KNW-2): for each
// catalog resource of kind corpus whose source is git (the default), the files of
// knowledge/<corpus>/ beside the tenant's catalog file. A corpus without such a folder is not
// sent, and the core keeps its documents (MAN-6).
func knowledgeFolders(docs []manifest.Doc, ms []map[string]any) []map[string]any {
	var out []map[string]any
	for i, d := range docs {
		if d.Kind != "Catalog" {
			continue
		}
		spec, _ := ms[i]["spec"].(map[string]any)
		res, _ := spec["resources"].([]any)
		for _, r := range res {
			m, _ := r.(map[string]any)
			id, _ := m["id"].(string)
			if m["kind"] != "corpus" || (m["source"] != nil && m["source"] != "git") || !skillNameRe.MatchString(id) {
				continue
			}
			dir := filepath.Join(filepath.Dir(d.Source), "knowledge", id)
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				continue
			}
			files, err := folderFiles(dir, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: knowledge/%s: %v\n", id, err)
				continue
			}
			out = append(out, map[string]any{"tenant": d.Tenant, "corpus": id, "documents": files})
		}
	}
	return out
}

// folderFiles reads the files under dir by their slash path. With modes, each file says whether
// it is a plain file, an executable or a symbolic link (whose target is not read); without, a
// link or another special file is left out.
func folderFiles(dir string, modes bool) ([]map[string]any, error) {
	var out []map[string]any
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		f := map[string]any{"path": filepath.ToSlash(rel)}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if !modes {
				return nil
			}
			f["mode"], f["data"] = "link", ""
			out = append(out, f)
			return nil
		case !info.Mode().IsRegular():
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		f["data"] = base64.StdEncoding.EncodeToString(b)
		if modes {
			f["mode"] = "file"
			if info.Mode()&0o111 != 0 {
				f["mode"] = "executable"
			}
		}
		out = append(out, f)
		return nil
	})
	return out, err
}
