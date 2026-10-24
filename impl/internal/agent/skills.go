// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// skillName is the folder name of a skill (the common name of the skeleton).
var skillName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// skillRef is one skill of the work item: name, description and tree hash (SKL-2).
type skillRef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SHA256      string `json:"sha256"`
}

// treeHash is the skill tree hash of spec/hashing.md over files by path, 64 hex digits.
func treeHash(files map[string]string) (string, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	arr := make([]any, 0, len(paths))
	for _, p := range paths {
		sum := sha256.Sum256([]byte(files[p]))
		arr = append(arr, map[string]any{"path": p, "sha256": "sha256:" + hex.EncodeToString(sum[:])})
	}
	h, err := jcs.Hash(arr)
	return strings.TrimPrefix(h, "sha256:"), err
}

// writeSkills puts the role's skills of an attempt into a new read-only directory under base,
// one folder per skill-ref (runtime.skills: runtime_files, SKL-2). Only the skills of the refs
// are written, each only when its files give the ref's tree hash; a file outside a ref has no
// place on the disk. It returns the directory and the section of the system prompt that names
// the skills and carries their SKILL.md text, which is reviewed text from the farm git. The
// section does not hold the directory, which is new for every attempt: the first message names
// it, so the system text of a role stays the same between attempts and the provider's prompt
// cache serves it (GW-16).
func writeSkills(base string, refs []skillRef, files map[string]map[string]string) (string, string, error) {
	if len(refs) == 0 {
		return "", "", nil
	}
	dir, err := os.MkdirTemp(base, "botaru-skills-")
	if err != nil {
		return "", "", err
	}
	var section strings.Builder
	section.WriteString("\n\nSkills of your role. Each is a procedure from the farm git; follow it when the task fits its description. Its files are in the skills directory that the first message names.")
	for _, r := range refs {
		fs := files[r.Name]
		if h, err := treeHash(fs); err != nil || h != r.SHA256 || !skillName.MatchString(r.Name) {
			_ = removeSkills(dir)
			return "", "", fmt.Errorf("the files of skill %s do not match its hash", r.Name)
		}
		for p, text := range fs {
			if !filepath.IsLocal(p) {
				_ = removeSkills(dir)
				return "", "", fmt.Errorf("skill %s has a path outside its folder", r.Name)
			}
			target := filepath.Join(dir, r.Name, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				_ = removeSkills(dir)
				return "", "", err
			}
			if err := os.WriteFile(target, []byte(text), 0o444); err != nil {
				_ = removeSkills(dir)
				return "", "", err
			}
		}
		fmt.Fprintf(&section, "\n\n## Skill %s\n%s\n\n%s", r.Name, r.Description, fs["SKILL.md"])
	}
	// The directory is read only for the run (SKL-2).
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o555)
		}
		return nil
	})
	if err != nil {
		_ = removeSkills(dir)
		return "", "", err
	}
	return dir, section.String(), nil
}

// removeSkills deletes a skill directory after its attempt, so the runtime keeps nothing of the
// attempt (RET-11).
func removeSkills(dir string) error {
	if dir == "" {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	err := os.RemoveAll(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
