// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SKL-2 on the runtime side (runtime.skills: runtime_files): only the skills of the refs are
// written, each only with its matching tree hash, into a read-only directory that is removed
// after the attempt; the prompt section carries the SKILL.md text. The hash is the example of
// examples/roles/ops-reader (spec/hashing.md).
func TestSKL2RuntimeSkillFiles(t *testing.T) {
	root := filepath.Join("..", "..", "..", "examples", "skills", "pipeline-tmp-cleanup")
	files := map[string]string{}
	for _, p := range []string{"SKILL.md", "references/checklist.md"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		files[p] = string(b)
	}
	h, err := treeHash(files)
	if err != nil || h != "680891b53fd090cbaa9f7a81f7ab901706135481e6ee7f71995631855c3bf7b7" {
		t.Fatalf("tree hash %s %v", h, err)
	}
	base := t.TempDir()
	refs := []skillRef{{Name: "pipeline-tmp-cleanup", Description: "Cleanup", SHA256: h}}
	all := map[string]map[string]string{"pipeline-tmp-cleanup": files, "other": {"SKILL.md": "not in the role"}}
	dir, section, err := writeSkills(base, refs, all)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section, files["SKILL.md"]) || strings.Contains(section, "not in the role") {
		t.Fatalf("section %q", section)
	}
	if _, err := os.Stat(filepath.Join(dir, "other")); !os.IsNotExist(err) {
		t.Fatal("a skill outside the role is on the disk")
	}
	for _, p := range []string{dir, filepath.Join(dir, "pipeline-tmp-cleanup", "references"), filepath.Join(dir, "pipeline-tmp-cleanup", "SKILL.md")} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm()&0o222 != 0 {
			t.Fatalf("%s is writable or missing: %v", p, err)
		}
	}
	if err := removeSkills(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the skill directory stays after the attempt")
	}
	// Files that do not give the ref's hash are not written at all.
	bad := map[string]map[string]string{"pipeline-tmp-cleanup": {"SKILL.md": "changed"}}
	if _, _, err := writeSkills(base, refs, bad); err == nil {
		t.Fatal("a skill of another hash was written")
	}
	if left, _ := os.ReadDir(base); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}
