// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// The methods of this file let another program run its own stand on a bench (the simulator,
// impl/SIM.md): it reads the store, plays people and presses buttons, adds workers and stops the
// spend; the eval runner does not use them.

// Store is the bench store.
func (b *Bench) Store() *store.Store { return b.st }

// Core is the bench core.
func (b *Bench) Core() *core.Core { return b.c }

// Seed is the seed the bench added its people from.
func (b *Bench) Seed() *Seed { return b.seed }

// P1 is the address of the P1 listener.
func (b *Bench) P1() string { return b.p1 }

// Enroll enrolls a worker through P2, of the tenant the request names (the home tenant without
// one), and returns its P1 token.
func (b *Bench) Enroll(ctx context.Context, req map[string]any) (string, error) {
	return b.enroll(ctx, req)
}

// Go runs a further worker until the bench closes.
func (b *Bench) Go(w func(context.Context) error) {
	b.done.Add(1)
	go func() { defer b.done.Done(); _ = w(b.runCtx) }()
}

// SpendTotal is the provider spend of all bench tenants in the store, in micro-euros.
func (b *Bench) SpendTotal(ctx context.Context) (int64, error) { return b.spendTotal(ctx) }

// StopSpend holds every bench tenant (no_enqueue, no_claim, cancel_running), so no task calls
// the model again; the holds stay until the bench closes.
func (b *Bench) StopSpend(ctx context.Context) error { return b.stopSpend(ctx) }

// Pending says whether a task of a tenant created since then is not terminal or a delivery is
// not done.
func (b *Bench) Pending(ctx context.Context, since time.Time, tenant string) (bool, error) {
	return b.pending(ctx, since, tenant)
}

// Tenants are the names of the bench tenants, the home first.
func (b *Bench) Tenants() []string { return b.tenantNames() }

// Home is the home tenant of the bench: the tenant of the eval cases and of the operator that
// applies the set.
func (b *Bench) Home() string { return b.home() }

// GoNamed runs a further worker under a name until StopWorker stops it or the bench closes; a
// name the bench already runs is refused.
func (b *Bench) GoNamed(name string, w func(context.Context) error) error {
	if !b.start(b.runCtx, name, w) {
		return fmt.Errorf("worker %s runs already", name)
	}
	return nil
}

// Workers are the names of the workers the bench started (<tenant>/<role>/<n> for an agent,
// <tenant>/telegram for a channel, <tenant>/toolhost/<host>/<n> for a toolhost), in name order.
func (b *Bench) Workers() []string {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return slices.Sorted(maps.Keys(b.workers))
}

// StopWorker stops one worker by name, as a crash would: its lease is left to expire. It says
// whether the bench knew the name.
func (b *Bench) StopWorker(name string) bool {
	b.workersMu.Lock()
	cancel, ok := b.workers[name]
	delete(b.workers, name)
	cut := b.cuts[name]
	b.workersMu.Unlock()
	if ok {
		if cut != nil {
			cut.Store(true)
		}
		cancel()
	}
	return ok
}

// RestartWorker starts again a bench worker that StopWorker stopped, as a new process of the
// same worker: the same enrollment and token. It fails for a name the bench never started or
// one that runs.
func (b *Bench) RestartWorker(name string) error {
	b.workersMu.Lock()
	run, cut := b.runs[name], b.cuts[name]
	_, running := b.workers[name]
	b.workersMu.Unlock()
	if run == nil {
		return fmt.Errorf("no bench worker %s", name)
	}
	if running {
		return fmt.Errorf("worker %s runs already", name)
	}
	if cut != nil {
		cut.Store(false)
	}
	if !b.start(b.runCtx, name, run) {
		return fmt.Errorf("worker %s runs already", name)
	}
	return nil
}

// WorkerName is the bench name of a worker by its id in the core, "" for a worker the bench
// did not start itself.
func (b *Bench) WorkerName(id string) string {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	for n, w := range b.workerIDs {
		if strings.EqualFold(w, id) {
			return n
		}
	}
	return ""
}

// StoreNow is the store clock.
func (b *Bench) StoreNow(ctx context.Context) (time.Time, error) { return b.storeNow(ctx) }

// identity gives the Telegram user id of a seed person, the id and kind of a seed chat and the
// fake Bot API of the person's tenant.
func (b *Bench) identity(from, chat string) (uid, cid int64, direct bool, bot *fakeBotAPI, err error) {
	p := b.seed.Person(from)
	ch := b.seed.Chat(chat)
	if p == nil || ch == nil {
		return 0, 0, false, nil, fmt.Errorf("no seed person %s or chat %s", from, chat)
	}
	if uid, err = strconv.ParseInt(telegramUserID(p.Identity), 10, 64); err != nil {
		return 0, 0, false, nil, fmt.Errorf("person %s has no telegram identity", from)
	}
	if cid, err = strconv.ParseInt(ch.ExternalID, 10, 64); err != nil {
		return 0, 0, false, nil, err
	}
	return uid, cid, ch.Kind == "direct", b.botOf(p.Tenant), nil
}

// Say queues a message of a seed person in a seed chat through the fake Bot API and returns the
// core's idempotency key of the activity; it does not wait for the work it makes.
func (b *Bench) Say(from, chat, text string) (string, error) {
	uid, cid, direct, bot, err := b.identity(from, chat)
	if err != nil {
		return "", err
	}
	id := bot.message(uid, cid, direct, text)
	return "system:activity:" + benchChannel + ":" + strconv.FormatInt(int64(id), 10), nil
}

// Press queues a press of a seed person on an inline button of a bot message in a seed chat.
func (b *Bench) Press(from, chat string, messageID int64, data string) error {
	uid, cid, direct, bot, err := b.identity(from, chat)
	if err != nil {
		return err
	}
	bot.press(uid, cid, messageID, direct, data)
	return nil
}

// Apply applies the bench set again: the Edit hook of the config runs anew, so a stand can
// change its manifests while it runs. With the Commit hook it writes the whole set as a new
// commit of the stand's git, which drops any change committed there since (a realized change
// would go without its Botaru-Revert line), so such a stand applies its git instead. It must
// not run alongside eval cases, which read the bench's profiles and result schemas.
func (b *Bench) Apply(ctx context.Context) error { return b.apply(ctx) }

// ResultSchema is the result schema of a task type of the bench set, nil without one.
func (b *Bench) ResultSchema(taskType string) map[string]any { return b.resultSchemas[taskType] }
