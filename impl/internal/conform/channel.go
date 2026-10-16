// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// The runner's own channel process (conformance/runner.md, actor channel:<name>): a channel
// worker of tenant demo that the runner enrolls with P2 worker.enroll, as an operator deploys a
// channel driver, and that submits activities and person calls over P1 like any driver. It is
// the path by which the runner plays a person in their chat: the /me command that shows the
// verification code of a privileged grant (PRM-13), and the steps a channel makes.

// benchChannel is the channel of the demo tenant in the base set (examples/tenants/demo/channels.yaml).
const benchChannel = "telegram"

// codeText is the key of the /me line that carries the verification code, in the code text
// catalog of the demo tenant's locale (spec/bindings/channels/messages/en.yaml).
const codeText = "cmd.profile.code"

// codePattern is the cmd.profile.code text as a pattern: the text around {code} literally, the
// code an 8-digit number, so another 8-digit number in the reply is never read as the code.
func codePattern(root string) (*regexp.Regexp, error) {
	b, err := os.ReadFile(filepath.Join(root, "spec/bindings/channels/messages/en.yaml"))
	if err != nil {
		return nil, err
	}
	var cat struct {
		Texts map[string]any `yaml:"texts"`
	}
	if err := yamlx.Unmarshal(b, &cat); err != nil {
		return nil, err
	}
	t := strings.TrimSpace(str(cat.Texts[codeText]))
	before, after, ok := strings.Cut(t, "{code}")
	if !ok {
		return nil, fmt.Errorf("the text %s has no {code}", codeText)
	}
	return regexp.Compile(regexp.QuoteMeta(before) + `([0-9]{8})` + regexp.QuoteMeta(after))
}

// channel returns the P1 token of the runner's channel worker, enrolling it on first use.
func (x *exec) channel(ctx context.Context) (string, error) {
	if x.chanToken != "" {
		return x.chanToken, nil
	}
	status, resp, err := x.post(ctx, x.ep.P2+"/v1/p2/worker.enroll", x.ep.Operators["demo"],
		map[string]any{"tenant": "demo", "role": benchChannel, "kind": "channel", "target": benchChannel})
	if err != nil {
		return "", err
	}
	if status != 200 {
		o := problemOutcome(status, resp)
		return "", fmt.Errorf("worker.enroll of the channel: %s%s", o.Reason, o.why())
	}
	tok := str(resp["token"])
	hello := map[string]any{"kind": "channel", "implementation": "botaru-conform", "capacity": 1, "protocol_versions": []any{"v1"},
		"driver": map[string]any{"ai_disclosure": "first_contact", "provenance": "visible_label", "stores_content": "none"}}
	status, resp, err = x.postP1(ctx, "hello", tok, nil, hello)
	if err != nil {
		return "", err
	}
	if status != 200 {
		o := problemOutcome(status, resp)
		return "", fmt.Errorf("hello of the channel: %s%s", o.Reason, o.why())
	}
	x.chanToken = tok
	return tok, nil
}

// postP1 calls a P1 operation with a worker token and the headers of a person's call.
func (x *exec) postP1(ctx context.Context, op, token string, headers map[string]string, body map[string]any) (int, map[string]any, error) {
	return x.postWith(ctx, x.ep.P1+"/v1/p1/"+op, token, headers, body)
}

// say submits a message of a person through the runner's channel: text in the chat with the
// external id ext (a direct chat when ext is the number of the person's identity).
func (x *exec) say(ctx context.Context, identity, chatKind, ext, text string) (int, map[string]any, error) {
	tok, err := x.channel(ctx)
	if err != nil {
		return 0, nil, err
	}
	x.updates++
	body := map[string]any{"kind": "message", "channel": benchChannel, "update_id": "conform-" + strconv.Itoa(x.updates),
		"sender": map[string]any{"identity": identity, "verified": true},
		"chat":   map[string]any{"kind": chatKind, "external_id": ext}, "text": text}
	return x.postP1(ctx, "submit_activity", tok, nil, body)
}

// directChat is the external id of a person's direct chat: the number of their channel
// identity (seed.yaml: alice-dm is 1001 for telegram:1001).
func directChat(identity string) string {
	n, ok := strings.CutPrefix(identity, "telegram:")
	if !ok {
		return ""
	}
	return n
}

// verificationCode plays /me in the person's direct chat and reads the verification code from
// the reply, as the person reads it and gives it to the operator (PRM-13).
func (x *exec) verificationCode(ctx context.Context, identity string) (string, error) {
	ext := directChat(identity)
	if ext == "" {
		return "", errUnchecked("the verification code of a person without a channel identity (PRM-13)")
	}
	status, resp, err := x.say(ctx, identity, "direct", ext, "/me")
	if err != nil {
		return "", err
	}
	if status != 200 {
		o := problemOutcome(status, resp)
		if o.Unimplemented != "" {
			return "", errUnchecked("core: " + o.Unimplemented)
		}
		return "", fmt.Errorf("/me: %s%s", o.Reason, o.why())
	}
	re, err := codePattern(x.r.Root)
	if err != nil {
		return "", err
	}
	m := re.FindStringSubmatch(str(resp["text"]))
	if m == nil {
		return "", fmt.Errorf("the /me reply carries no verification code: %q", str(resp["text"]))
	}
	return m[1], nil
}
