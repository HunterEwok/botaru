// SPDX-License-Identifier: AGPL-3.0-only

// Package yamlx reads YAML the way the skeleton means it: a date such as 2026-10-01 stays the
// string it is in the file. The YAML library would otherwise turn it into a timestamp, and the
// schemas check dates as strings.
package yamlx

import (
	"bytes"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"
)

// Decoder reads the documents of a YAML stream.
type Decoder struct{ d *yaml.Decoder }

// NewDecoder returns a decoder of r.
func NewDecoder(r io.Reader) *Decoder { return &Decoder{d: yaml.NewDecoder(r)} }

// Decode reads the next document into v; it returns io.EOF after the last one.
func (d *Decoder) Decode(v any) error {
	var n yaml.Node
	if err := d.d.Decode(&n); err != nil {
		return err
	}
	plain(&n)
	return n.Decode(v)
}

// Unmarshal reads one YAML document into v.
func Unmarshal(b []byte, v any) error {
	err := NewDecoder(bytes.NewReader(b)).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func plain(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!timestamp" {
		n.Tag = "!!str"
	}
	for _, c := range n.Content {
		plain(c)
	}
}
