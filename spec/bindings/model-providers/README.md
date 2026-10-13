# Model provider bindings

## In short

P4 has no named operations: the runtime talks to P4 in the provider's format through a model handle, so the P4 contract for each provider is its binding. The binding says which request fields P4 lets through (the allowlist, GW-1), how P4 sets the model, the region and the cache point, how it computes usage from the response and how it maps provider errors to `reasons.yaml` codes. There is one reference provider in v0.1: the Anthropic Messages API (`anthropic.md`, lead decision 2 of the 2026-10-12 review). Another provider is added as a new file in this folder with the same content; sheet P4 and the core do not change. The mock provider for the runner is described in `mock.md`.

## What a provider binding must describe

| Section | What it holds | Rules |
|---|---|---|
| Version | the provider API version, the HTTP binding path (`/v1/p4/<provider>/...`), the date it was checked | |
| Paths and address | the closed list of provider paths and methods that P4 opens (all others `404 not_found`), and the closed list of official provider addresses; another address only on the test bench | GW-1, GW-3 |
| Headers | the closed list of headers that P4 itself sets towards the provider; runtime headers do not reach the provider | GW-1, GW-3 |
| Allowlist | the closed request schema `spec/model/p4-<provider>-<operation>-request.schema.json`: the fields P4 lets through; everything outside the list, provider server tools, connecting external tool servers, containers and the region field are rejected | GW-1, INJ-9 |
| Model | which field names the model and how P4 sets the model of the handle's slot profile in it | GW-7, GW-5 |
| Tools | how the request's `tools` are compared with the P3 catalog of the handle by names and schema hashes (`spec/hashing.md`) | GW-1 |
| Output limit | which field limits the response and how P4 sets it no higher than the profile's `max_output_tokens` | GW-2 |
| Region | the request field or workspace setting through which P4 sets the region from `privacy.subprocessors[].region` | GW-14 |
| Cache | the cache control fields in the allowlist and the rule for where P4 sets the cache point | GW-16 |
| Usage | which response fields give tokens by kind (input, output, cache write and read, reasoning, compaction iterations), how they are converted to micro-euros by the `ModelProfile` prices, the "model answered" flag | BUD-2, BUD-3, BUD-9, GW-13 |
| Stream | how P4 computes usage if the runtime closed the stream before the final `usage`: P4 reads the stream to the end and counts what was actually sent, otherwise counts by the reserve | GW-2, BUD-11 |
| Errors | the table "provider answer -> `reason`", which errors open the circuit, when one client retry is allowed | GW-11, GW-12, GW-13, CAP-2 |
| Reconciliation report | which provider report serves reconciliation and for which period (daily total per workspace) | BUD-10 |
| Region outside the EU | which regions of the list lie outside the EU: a production tenant with such a region needs a transfer basis `Tenant.spec.model_transfer` (D-87 (2)) | GW-14 |
| Key and workspace | how P4 holds the tenant's key and workspace and how it checks the ceilings daily | GW-3, GW-9 |
| Checks | what the mock provider `mock.md` must be able to do for this binding | |

A binding is not a transparent proxy. A binding introduces no new reasons or record fields: a reason code comes only from `spec/model/reasons.yaml`, a usage line only follows `spec/model/spend.schema.json`.
