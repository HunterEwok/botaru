// SPDX-License-Identifier: AGPL-3.0-only

// Package testbench prepares the initial state of the hello-bot example for tests: the
// manifests of examples/hello-bot/sequence.yaml with the demo tenant switched to kind: test,
// as conformance/runner.md ("Initial state") asks, and the model profile of the role slot.
package testbench

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// HelloBotFiles are the manifests of the hello-bot sequence, relative to the skeleton root.
var HelloBotFiles = []string{
	"examples/hello-bot/role.yaml",
	"examples/hello-bot/tasktype.yaml",
	"examples/hello-bot/group.yaml",
	"examples/tenants/demo/tenant.yaml",
	"examples/tenants/demo/channels.yaml",
	"examples/model-profiles/base-std.yaml",
}

// HelloBotManifests reads the hello-bot manifests, rewrites every Tenant to kind: test and
// drops the fields that a core of this version refuses (DropUnsupported).
func HelloBotManifests(root string) ([]map[string]any, error) {
	paths := make([]string, len(HelloBotFiles))
	for i, f := range HelloBotFiles {
		paths[i] = filepath.Join(root, f)
	}
	docs, err := manifest.ReadFiles(paths)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		if d.Kind == "Tenant" {
			if spec, ok := d.Value["spec"].(map[string]any); ok {
				spec["kind"] = "test"
			}
		}
		out = append(out, d.Value)
	}
	sp, err := spec.Load(root)
	if err != nil {
		return nil, err
	}
	if _, err := DropUnsupported(sp, out); err != nil {
		return nil, err
	}
	return out, nil
}

// DropUnsupported removes from a manifest set, in place, the fields that apply of this core
// refuses with version_unsupported (MAN-11), and returns their paths in the form of apply
// errors. The example tenant carries fields of later versions (locale, retention, the
// operator_access of the debug profile, the channel rate), which the hello-bot sequence does
// not need. Only a whole optional field is left out: a later kind, or a value that gives a
// field its meaning (a catalog resource kind, a channel direction, a tier), stops it with an
// error, because without it the manifest would say something else.
func DropUnsupported(sp *spec.Spec, ms []map[string]any) ([]string, error) {
	var dropped []string
	for i, m := range ms {
		ptrs, err := sp.Unsupported(m)
		if err != nil {
			return nil, err
		}
		for _, p := range ptrs {
			if err := sp.Droppable(m, p); err != nil {
				return nil, fmt.Errorf("manifest %d (%v %v): %w", i, m["kind"], metaName(m), err)
			}
		}
		for _, p := range ptrs {
			if !manifest.DropPointer(m, p) {
				return nil, fmt.Errorf("manifest %d (%v %v): %s cannot be left out", i, m["kind"], metaName(m), p)
			}
			dropped = append(dropped, "/manifests/"+strconv.Itoa(i)+p)
		}
	}
	return dropped, nil
}

func metaName(m map[string]any) any {
	md, _ := m["metadata"].(map[string]any)
	return md["name"]
}

// ExampleActions reads the actions of examples/actions.yaml without the fields this core
// refuses as later (DropUnsupported).
func ExampleActions(root string) ([]map[string]any, error) {
	docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/actions.yaml")})
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, d := range docs {
		if d.Kind == "Action" {
			out = append(out, d.Value)
		}
	}
	sp, err := spec.Load(root)
	if err != nil {
		return nil, err
	}
	if _, err := DropUnsupported(sp, out); err != nil {
		return nil, err
	}
	return out, nil
}
