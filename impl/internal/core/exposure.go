// SPDX-License-Identifier: AGPL-3.0-only

package core

import "github.com/HunterEwok/botaru/impl/internal/toolspec"

// modelExposure is the model_exposure of a catalog resource (PRV-4): a dataset without the
// field counts as aggregates; another resource without it has no exposure rule ("").
func modelExposure(resource map[string]any) string {
	if e, ok := resource["model_exposure"].(string); ok {
		return e
	}
	if resource["kind"] == "dataset" {
		return "aggregates"
	}
	return ""
}

// exposureAllows says whether a read tool of an agent role may read a resource of the tenant
// catalog (PRV-4): none never, aggregates only by a tool whose output is numbers and enum
// values by its schema (untrusted_output: false, INJ-3), full or no rule by any read tool
// within the data class.
func exposureAllows(cat *Catalog, tenant, resource string, t toolspec.Tool) bool {
	if resource == "" {
		return true
	}
	spec, _ := tenantCatalog(cat, tenant)
	res, _ := spec["resources"].([]any)
	for _, r := range res {
		rm, _ := r.(map[string]any)
		if rm["id"] != resource {
			continue
		}
		switch modelExposure(rm) {
		case "none":
			return false
		case "aggregates":
			return t.TrustedOutput()
		}
	}
	return true
}
