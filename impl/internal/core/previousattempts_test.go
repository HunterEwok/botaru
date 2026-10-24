// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// A retry hears how the earlier attempts of the task ended, without the provider's failures,
// which the model could not have avoided (extension field previous_attempts).
func TestPreviousAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	att := claimFor(t, b, b.agent, task)
	if _, ok := att.Payload["previous_attempts"]; ok {
		t.Fatal("the first attempt names earlier ones")
	}
	if _, err := b.c.Fail(ctx, b.agent, header(t, att), FailRequest{Reason: "provider_unavailable", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `UPDATE tasks SET run_at = botaru_now() WHERE id = $1`, task)
	b.exec(t, `DELETE FROM provider_pairs`)
	att = claimFor(t, b, b.agent, task)
	if _, ok := att.Payload["previous_attempts"]; ok {
		t.Fatal("a provider failure reached the model")
	}
	if _, err := b.c.Fail(ctx, b.agent, header(t, att), FailRequest{Reason: "model_error", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `UPDATE tasks SET run_at = botaru_now() WHERE id = $1`, task)
	att = claimFor(t, b, b.agent, task)
	prev, _ := att.Payload["previous_attempts"].([]map[string]any)
	if len(prev) != 1 || prev[0]["reason"] != "model_error" {
		t.Fatalf("previous_attempts %#v", att.Payload["previous_attempts"])
	}
}
