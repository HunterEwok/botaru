# Lifecycle: worker

## In short

A worker is a process that takes work through P1 `claim`: an agent, a pipeline (`job`), a toolhost, a channel, a sink or a monitoring adapter (`monitor`, E5). The operator enrolls it with the P2 command `worker.enroll`: the credentials fix the tenant, the role, the kind, the labels, for a toolhost also the plugins, the placement and the machine, and for a personal assistant the owner `owner`, the person it serves (IDN-5). The first `hello` that matches the record makes the worker operational; a mismatch blocks it until fixed. A worker's liveness is not a state but the `last_seen` mark at the core. A bot (an identity with a number) is not a worker: the identity is bound to a worker and survives its replacement.

## States

| State | Meaning | Terminal |
|---|---|---|
| `enrolled` | The operator enrolled the worker and issued a token; `hello` has not passed yet. No work | no |
| `active` | `hello` matched the `enroll` record; the worker takes work through `claim` | no |
| `blocked` | `hello` brought something other than what is recorded (plugins, placement, the `tools/list` fingerprint, labels), or the credentials gave a series of `tenant_mismatch` or of someone else's handles. No work | no |
| `draining` | A hold of the `worker` scope with the effect `no_claim`: no new work, running work is finished | no |
| `revoked` | The token, the leases and the handles are revoked; the worker no longer exists for the core | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| none | `enrolled` | P2 `worker.enroll` | the operator's right `admin:workers` in `farm`; the worker's tenant is set by the `--tenant` parameter. With the `--bot` flag the worker is bound to an existing identity (epoch + 1), without the flag the core creates a new identity | TEN-4, DEP-2, IDN-4, IDN-8 |
| `enrolled` | `active` | P1 `hello` | the kind, the labels (only narrowing), the plugins, the placement and the `tools/list` fingerprint matched the record and the manifests; the protocol version is within the window | DEP-2, INJ-2 |
| `enrolled`, `active` | `blocked` | P1 `hello` | a mismatch with the record or the manifest: a different set of plugins, a different placement, the `tools/list` fingerprint did not match, labels wider than recorded | DEP-2, INJ-2, BGR-1 |
| `active` | `blocked` | the core | a series of `tenant_mismatch` or of someone else's handles from these credentials | TEN-2 |
| `blocked` | `active` | P2 `worker.unblock`, then P1 `hello` | the operator fixed the manifest or issued a new `enroll`, and the next `hello` matched | DEP-2 |
| `active` | `draining` | P2 `worker.drain`; also the control operation `drain` | a hold of the `worker` scope with the effect `no_claim` is set | HLD-2, HLD-3, MGR-9 |
| `draining` | `active` | P2 `hold.release` of this hold, then P1 `hello` | the hold is released, `hello` matched | HLD-3 |
| `draining` | `draining` | the `drain_deadline` (v0.2) | running work did not finish by the deadline: the worker lets it go with `release`, the attempt is not spent | LCY-4 |
| `active` | `active` | P2 `worker.rotate` | a new token; the old one lives for a limited window | IDN-4 |
| `enrolled`, `active`, `blocked`, `draining` | `revoked` | P2 `worker.revoke` | the leases and the handles are revoked in the same transaction; the bound identity becomes `unbound`; from v0.2 a `runtime_internal` runtime profile is cleaned through the adapter | IDN-4, LCY-4, RT-1, RT-2 |

## Invariants

- A worker serves exactly one tenant, recorded at `enroll`; `hello` can only narrow the recorded labels (TEN-4, DEP-2).
- Only a worker in `active` gets work, only through `claim` and only of its own tenant (DSP-1, TEN-3). In `enrolled`, `blocked`, `draining` and `revoked` there is no new work.
- Liveness is `last_seen`, which any P1 call updates, and the `worker.stale` event of the `worker-liveness` loop, not a state. The worker's self-report is for reference only (STA-2).
- At any moment one identity is bound to at most one worker in `active` or `draining` (IDN-8).
- A hold of the `worker` scope with other effects (a quarantine) changes what the worker can take, but not its state; only a drain moves a worker to `draining`.
- An identity that stayed `unbound` for longer than 30 days retires with the reason `worker_gone` (IDN-11).
- The worker token is stored as a hash and is not accepted on P2 (IDN-4, IDN-7).

## Versions

| Version | What appears |
|---|---|
| v0.1 | The states and transitions above without drain deadlines; `last_seen` and the `worker.stale` event; binding a bot identity at `enroll` (`--bot`) and at `revoke`; the control operation `drain`; the P1 version window (the current and the previous major version) |
| v0.2 | The `drain_deadline`; cleaning the runtime profile at `revoke`; accepted plugin pairs (digest and fingerprint); the health report in `hello` and `heartbeat` |
