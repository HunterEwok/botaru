// SPDX-License-Identifier: AGPL-3.0-only

package toolhost

import "testing"

// Every write to the outside world carries a work id shared by all attempts: the approval id of
// an action, <approval_id>:<n> for a campaign item, the service_grant record of a service or
// script call; another call carries its own id (FEN-7).
func TestBackfillCallWorkID(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    callPayload
		want string
	}{
		{"action", callPayload{ApprovalID: "ap_17"}, "ap_17"},
		{"campaign item", callPayload{ApprovalID: "ap_17", CampaignItem: 3}, "ap_17:3"},
		{"service control", callPayload{GrantID: "sg-5"}, "sg-5"},
		{"read", callPayload{}, "tc_1"},
	} {
		// FEN-7: the same key for every attempt of the write.
		if got := callWorkID("tc_1", tc.c); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if again := callWorkID("tc_2", tc.c); tc.c.ApprovalID+tc.c.GrantID != "" && again != tc.want {
			t.Errorf("%s: another attempt has the key %q", tc.name, again)
		}
	}
}
