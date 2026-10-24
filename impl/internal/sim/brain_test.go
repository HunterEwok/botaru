// SPDX-License-Identifier: AGPL-3.0-only

package sim

import "testing"

// The mock plans from the task's own goal, without the earlier messages of the chat.
func TestWithoutConversation(t *testing.T) {
	for in, want := range map[string]string{
		`Why? <m>{"conversation":[{"role":"user","text":"disk [x]"}],"facts":[]}</m>`: `Why? <m>{"facts":[]}</m>`,
		`Why? <m>{"attachments":[],"conversation":[{"text":"a,b"}]}</m>`:              `Why? <m>{"attachments":[]}</m>`,
		`Why? <m>{"conversation":[]}</m>`:                                             `Why? <m>{}</m>`,
		`Why?`:                                                                        `Why?`,
	} {
		if got := withoutConversation(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
