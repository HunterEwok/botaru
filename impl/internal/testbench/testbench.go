// SPDX-License-Identifier: AGPL-3.0-only

// Package testbench prepares the initial state of the hello-bot example for tests: the
// manifests of examples/hello-bot/sequence.yaml with the demo tenant switched to kind: test,
// as conformance/runner.md ("Initial state") asks, and the model profile of the role slot.
package testbench

import (
	"path/filepath"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
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

// HelloBotManifests reads the hello-bot manifests and rewrites every Tenant to kind: test.
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
	return out, nil
}
