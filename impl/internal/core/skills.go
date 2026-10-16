// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Role skills (profile knowledge, SKL-1 to SKL-3, docs/capabilities.md "Skills"). A skill is a
// folder skills/<name>/ of the farm git that apply carries with the set; the core lints it,
// keeps its own copy by the tree hash of spec/hashing.md and gives a run only the skills its
// role pins with a matching hash, as references in the work item and as files for a runtime
// that declares runtime.skills: runtime_files. The other delivery, skills: farm with the
// built-in skill_read, is deferred (SKL-4) and refused at hello. A skill is text: it adds no
// tool, tier, data class or right (SKL-3), so nothing here touches the P3 catalog.

// SkillFile is one entry of a skill folder in P2 apply: its path in skills/<name>/, the bytes as
// git stores them (base64 on the wire) and the git mode (file, executable or link).
type SkillFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
	Mode string `json:"mode,omitempty"`
}

// Limits of a skill folder (SKL-1).
const (
	skillMaxFiles     = 50
	skillMaxMain      = 32 << 10
	skillMaxTree      = 256 << 10
	skillMainFile     = "SKILL.md"
	skillRefsFolder   = "references/"
	skillAssetsFolder = "assets/"
)

// skillTypes are the file types a skill holds: UTF-8 text only (SKL-1).
var skillTypes = []string{".md", ".txt", ".yaml", ".json", ".csv"}

// lintSkill checks one skill folder by SKL-1 and returns what breaks the format, in path
// order: only SKILL.md, references/ and assets/, UTF-8 text files of the allowed types, no
// executable file or symbolic link (so no scripts/ either), at most 50 files, SKILL.md up to
// 32 KiB and the tree up to 256 KiB, and a front matter that passes the skill schema (which has
// no allowed-tools) with the folder's name.
func (c *Core) lintSkill(name string, files []SkillFile) []string {
	var out []string
	paths := map[string]bool{}
	var main []byte
	total := 0
	sorted := slices.Clone(files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	if len(files) > skillMaxFiles {
		out = append(out, fmt.Sprintf("%d files, at most %d", len(files), skillMaxFiles))
	}
	for _, f := range sorted {
		p := f.Path
		switch {
		case !cleanSkillPath(p):
			out = append(out, fmt.Sprintf("%q is not a plain relative path", p))
			continue
		case paths[p]:
			out = append(out, fmt.Sprintf("%s is listed twice", p))
			continue
		}
		paths[p] = true
		if p != skillMainFile && !strings.HasPrefix(p, skillRefsFolder) && !strings.HasPrefix(p, skillAssetsFolder) {
			out = append(out, fmt.Sprintf("%s is outside SKILL.md, references/ and assets/", p))
		}
		switch f.Mode {
		case "", "file":
		case "link":
			out = append(out, fmt.Sprintf("%s is a symbolic link", p))
			continue
		case "executable":
			out = append(out, fmt.Sprintf("%s is executable", p))
		default:
			out = append(out, fmt.Sprintf("%s has the unknown mode %s", p, f.Mode))
		}
		if !slices.ContainsFunc(skillTypes, func(ext string) bool { return strings.HasSuffix(p, ext) }) {
			out = append(out, fmt.Sprintf("%s has a type outside %s", p, strings.Join(skillTypes, ", ")))
		}
		if !utf8.Valid(f.Data) || bytes.IndexByte(f.Data, 0) >= 0 {
			out = append(out, fmt.Sprintf("%s is not UTF-8 text", p))
		}
		total += len(f.Data)
		if p == skillMainFile {
			main = f.Data
			if len(f.Data) > skillMaxMain {
				out = append(out, "SKILL.md is over 32 KiB")
			}
		}
	}
	if total > skillMaxTree {
		out = append(out, "the tree is over 256 KiB")
	}
	if main == nil {
		return append(out, "no SKILL.md")
	}
	fm, ok := skillFrontMatter(main)
	if !ok {
		return append(out, "SKILL.md has no YAML front matter")
	}
	if err := c.spec.Validate("manifest:skill", fm); err != nil {
		out = append(out, "the front matter fails the skill schema: "+err.Error())
	}
	if fm["name"] != name {
		out = append(out, fmt.Sprintf("the front matter name %v differs from the folder name", fm["name"]))
	}
	return out
}

// cleanSkillPath says whether a path is relative, with / as the only separator and no empty,
// "." or ".." segment.
func cleanSkillPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || len(p) > 400 {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// skillFrontMatter reads the YAML front matter of SKILL.md: the block between a first line
// "---" and the next line "---".
func skillFrontMatter(main []byte) (map[string]any, bool) {
	text := strings.ReplaceAll(string(main), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return nil, false
	}
	block, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		if block, ok = strings.CutSuffix(rest, "\n---"); !ok {
			return nil, false
		}
	}
	var fm map[string]any
	if err := yamlx.Unmarshal([]byte(block), &fm); err != nil || fm == nil {
		return nil, false
	}
	return fm, true
}

// skillTreeHash is the skill tree hash of spec/hashing.md: the files in ascending order of the
// path bytes, each as {path, sha256} with the hash of the file bytes in the sha256:<hex>
// notation, hashed in the canonical form. The role's skill-ref writes it as the 64 hexadecimal
// digits without the prefix, as examples/roles/ops-reader pins examples/skills/.
func skillTreeHash(files []SkillFile) (string, error) {
	sorted := slices.Clone(files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	arr := make([]any, 0, len(sorted))
	for _, f := range sorted {
		sum := sha256.Sum256(f.Data)
		arr = append(arr, map[string]any{"path": f.Path, "sha256": "sha256:" + hex.EncodeToString(sum[:])})
	}
	h, err := jcs.Hash(arr)
	return strings.TrimPrefix(h, "sha256:"), err
}

// skillTrees lints the skill folders of an apply (SKL-1) and returns their tree hashes by name.
// A folder that breaks the format fails the set with skill_format on /skills/<name>.
func (c *Core) skillTrees(sent map[string][]SkillFile) (map[string]string, []ApplyError, error) {
	hashes := map[string]string{}
	var errs []ApplyError
	for _, name := range slices.Sorted(mapKeys(sent)) {
		if probs := c.lintSkill(name, sent[name]); len(probs) > 0 {
			for _, p := range probs {
				errs = append(errs, ApplyError{Code: "skill_format", Path: "/skills/" + name, Detail: p})
			}
			continue
		}
		h, err := skillTreeHash(sent[name])
		if err != nil {
			return nil, nil, err
		}
		hashes[name] = h
	}
	return hashes, errs, nil
}

// skillPins warns about each skill-ref of a role in the set whose tree is neither among the
// folders of this apply nor in the core's copy: no run of the role starts until an apply brings
// the tree of that hash (SKL-2). The skill-ref's description must be the front matter's.
func (c *Core) skillPins(ctx context.Context, docs []manifest.Doc, index []int, sent map[string][]SkillFile, hashes map[string]string) ([]ApplyError, error) {
	var warns []ApplyError
	for j, d := range docs {
		if d.Kind != "Role" || index[j] < 0 {
			continue
		}
		sp, _ := d.Value["spec"].(map[string]any)
		for k, r := range mlist(sp, "skills") {
			ref, _ := r.(map[string]any)
			name, sha := mstr(ref, "name"), mstr(ref, "sha256")
			path := fmt.Sprintf("/manifests/%d/spec/skills/%d", index[j], k)
			if hashes[name] == sha {
				if fm, ok := skillFrontMatter(skillMain(sent[name])); ok && fm["description"] != mstr(ref, "description") {
					warns = append(warns, ApplyError{Kind: d.Kind, Name: d.Name, Code: "schema_invalid", Path: path + "/description",
						Detail: "the description differs from the front matter of SKILL.md"})
				}
				continue
			}
			var kept bool
			if err := c.st.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM skill_trees WHERE name = $1 AND sha256 = $2)`, name, sha).Scan(&kept); err != nil {
				return nil, err
			}
			if !kept {
				warns = append(warns, ApplyError{Kind: d.Kind, Name: d.Name, Code: "not_found", Path: path,
					Detail: "no skill folder of this name has this tree hash; the role gets no run until one does (SKL-2)"})
			}
		}
	}
	return warns, nil
}

func skillMain(files []SkillFile) []byte {
	for _, f := range files {
		if f.Path == skillMainFile {
			return f.Data
		}
	}
	return nil
}

// storeSkillTrees keeps the core copy of the linted skill folders of an apply by tree hash.
func storeSkillTrees(ctx context.Context, tx pgx.Tx, now time.Time, sent map[string][]SkillFile, hashes map[string]string) error {
	for name, h := range hashes {
		files := map[string]string{}
		for _, f := range sent[name] {
			files[f.Path] = string(f.Data)
		}
		raw, err := json.Marshal(files)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO skill_trees (name, sha256, files, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
			name, h, raw, now); err != nil {
			return err
		}
	}
	return nil
}

// errSkillMissing: a skill-ref of the role has no core copy of its hash.
var errSkillMissing = errors.New("a pinned skill has no core copy of its hash")

// roleSkills are the skills a run of the role gets (SKL-2): the role's skill-refs, each with the
// files of the core copy whose tree hash equals the ref, recomputed from the copy, so a copy
// that no longer matches its key is not given either. errSkillMissing reports a ref without
// such a copy: the run gets no skills of another hash, and no run starts.
func roleSkills(ctx context.Context, q store.Querier, role manifest.Role) ([]any, map[string]map[string]string, error) {
	refs := []any{}
	files := map[string]map[string]string{}
	for _, r := range role.Skills {
		var raw []byte
		err := q.QueryRow(ctx, `SELECT files FROM skill_trees WHERE name = $1 AND sha256 = $2`, r.Name, r.SHA256).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, errSkillMissing
		}
		if err != nil {
			return nil, nil, err
		}
		var fs map[string]string
		if err := json.Unmarshal(raw, &fs); err != nil {
			return nil, nil, err
		}
		tree := make([]SkillFile, 0, len(fs))
		for p, text := range fs {
			tree = append(tree, SkillFile{Path: p, Data: []byte(text)})
		}
		if h, err := skillTreeHash(tree); err != nil || h != r.SHA256 {
			return nil, nil, errSkillMissing
		}
		refs = append(refs, map[string]any{"name": r.Name, "description": r.Description, "sha256": r.SHA256})
		files[r.Name] = fs
	}
	return refs, files, nil
}

// skillsPart is the skills part of the run and the static fingerprint (spec/hashing.md): the
// hash of the array of the role's skill tree hashes in ascending order, each in the sha256:<hex>
// notation; the hash of the empty array for a role without skills (PRV-1, PRV-3).
func skillsPart(refs []manifest.SkillRef) (string, error) {
	hs := make([]string, 0, len(refs))
	for _, r := range refs {
		hs = append(hs, "sha256:"+r.SHA256)
	}
	sort.Strings(hs)
	arr := make([]any, len(hs))
	for i, h := range hs {
		arr[i] = h
	}
	return jcs.Hash(arr)
}

// skillsRefusal says why a hello cannot serve the role's skills, or "": skills: farm is the
// deferred delivery of SKL-4, and an absent value means farm, which a role with skills cannot
// use (SKL-2). Both answer version_unsupported, as for any mechanism this core does not offer.
func skillsRefusal(r *RuntimeDecl, role manifest.Role) string {
	switch {
	case r != nil && r.Skills == "farm":
		return "runtime.skills: farm and skill_read are not offered by this core (SKL-4)"
	case len(role.Skills) > 0 && knowledgeOn() && (r == nil || r.Skills != "runtime_files"):
		return "the role has skills, and this core gives them only to a runtime with runtime.skills: runtime_files (SKL-2)"
	}
	return ""
}

// skillRefsOf reads the skill-refs of a role spec as a manifest holds it.
func skillRefsOf(roleSpec map[string]any) []manifest.SkillRef {
	var out []manifest.SkillRef
	for _, r := range mlist(roleSpec, "skills") {
		m, _ := r.(map[string]any)
		out = append(out, manifest.SkillRef{Name: mstr(m, "name"), Description: mstr(m, "description"), SHA256: mstr(m, "sha256")})
	}
	return out
}
