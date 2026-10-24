// SPDX-License-Identifier: AGPL-3.0-only

package ijson

import (
	"encoding/json"
	"testing"
)

// Fuzzing the strict parser: it never panics, and what it accepts it accepts again after a
// round trip through encoding/json.
func FuzzParse(f *testing.F) {
	for _, s := range []string{`{"a":[1,2.5,"x",true,null]}`, `{"a":1,"a":2}`, `"\ud800"`, `1e400`, `[[[[[[[[[[]]]]]]]]]]`, "{\"a\":\"\x00\"}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data)
		if err != nil {
			return
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%q parsed to %#v, which does not marshal: %v", data, v, err)
		}
		if _, err := Parse(out); err != nil {
			t.Fatalf("%q -> %s is refused: %v", data, out, err)
		}
	})
}
