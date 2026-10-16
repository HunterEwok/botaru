// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// LoadChannel reads the Channel manifest of a source channel of a tenant from the tenant's git
// files: the driver applies the ingress block it holds (ING-1 to ING-3).
func LoadChannel(paths []string, tenant, name string) (manifest.Channel, error) {
	docs, err := manifest.ReadFiles(paths)
	if err != nil {
		return manifest.Channel{}, err
	}
	for _, d := range docs {
		if d.Kind == "Channel" && d.Tenant == tenant && d.Name == name {
			return manifest.Decode[manifest.Channel](d)
		}
	}
	return manifest.Channel{}, fmt.Errorf("no Channel %s of tenant %s in the manifests", name, tenant)
}

// LoadFormats reads the value formats of spec/model/formats.yaml. A pattern that the Go regular
// expressions do not take is left out, so its leaves pass no value in the driver; the core checks
// them again in any case.
func LoadFormats(sp *spec.Spec) (map[string]Format, error) {
	reg, err := sp.Registry("spec/model/formats.yaml")
	if err != nil {
		return nil, err
	}
	out := map[string]Format{}
	for _, e := range reg.Entries {
		id, _ := e["id"].(string)
		pat, _ := e["pattern"].(string)
		re, err := regexp.Compile(pat)
		if id == "" || pat == "" || err != nil {
			continue
		}
		n, _ := e["max_length"].(int)
		out[id] = Format{Pattern: re, MaxLength: n}
	}
	return out, nil
}

// SigningKey decodes a Standard Webhooks secret whsec_<base64> into its key.
func SigningKey(secret string) ([]byte, error) {
	b64, ok := strings.CutPrefix(strings.TrimSpace(secret), "whsec_")
	if !ok {
		return nil, errors.New("a signing secret has the form whsec_<base64>")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(key) < 16 {
		return nil, errors.New("the signing secret does not decode to a key of at least 16 bytes")
	}
	return key, nil
}

// TokenHash is the SHA-256 the driver keeps of a source token: a token of at least 32 bytes, or
// the hash itself in 64 hexadecimal digits prefixed with sha256: (webhook.md).
func TokenHash(secret string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	secret = strings.TrimSpace(secret)
	if h, ok := strings.CutPrefix(secret, "sha256:"); ok {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != sha256.Size {
			return out, errors.New("a token hash is sha256: and 64 hexadecimal digits")
		}
		copy(out[:], b)
		return out, nil
	}
	if len(secret) < 32 {
		return out, errors.New("a source token has at least 32 bytes")
	}
	return sha256.Sum256([]byte(secret)), nil
}
