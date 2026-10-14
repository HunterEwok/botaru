# Bindings

## In short

A binding is the place where a port sheet or a rule meets a concrete technology: HTTP paths, provider field names, OS units, the tool protocol revision, monitoring system items. Port sheets and registry rules say "tool protocol", "OS mechanism", "monitoring system", "model provider" and name no products; names, versions and exact shapes live here. So replacing a technology means a new binding folder or file with the same contract, not an edit of the sheet and not an edit of the core. Every binding says three things: what it implements (which sheet and which of its operations), from which skeleton version it is required and which `conformance/` checks hold it.

## Folders

| Folder | What it binds | Sheet | Since | Reference |
|---|---|---|---|---|
| `http/` | the wire envelope of P1, P2, the P3 and P4 transport and the test-control port: `/v1/...` paths, lease token and handle headers, acting on behalf of a person, long-poll waiting, files, problem details and status codes, idempotency (`README.md`, `admin.md`); bodies are `spec/model` schemas by the map `spec/ports/operations.yaml`; the P1 OpenAPI 3.1 description `http/p1.openapi.json`, generated from the map, referencing the same schemas from v0.2 (MAN-12) | P1, P2, P3, P4, test-control port | envelope v0.1, OpenAPI v0.2 | HTTP/1.1 and HTTP/2, RFC 9457 |
| `mcp/` | the tool protocol: revision, sessionless mode, what of the protocol is used on the north side of P3 and on the toolhost side towards plugins | P3, E3 | v0.1 | MCP, revision 2026-07-28 or later |
| `os/` | the OS mechanisms `oneshot` and `unit` for write calls and catalog services: units, the access rule that `apply` generates, where the rule sits on the machine | E3 | `oneshot` and `unit` v0.1 | `systemd.md` |
| `model-providers/` | the request field allowlist, model, output limit, region, cache, usage and provider error codes (`README.md`); the runner's mock provider (`mock.md`) | P4 | v0.1 | `anthropic.md`, Anthropic Messages API |
| `events/` | the CloudEvents event catalog (`catalog.yaml`): `ext.*` and `alarm.*` types, data schemas | P1 `emit`, E1, E5 | v0.1 | CloudEvents 1.0 |
| `channels/` | how channel fields map onto `Activity` and `Delivery`, authenticity, chat membership; for a source channel, the incoming format | E1 | v0.1 | `telegram.md` |
| `telemetry/` | the allowlist of telemetry attributes without content (OBS-1) | all processes | v0.1 | the deployment chooses the collector |
| `monitor/` | items, triggers, tags, webhook, scripts and notes of the monitoring system | E5 | v0.1 | `zabbix.md` (the reference is described in sheet E5) |

## Rules for a binding

- A binding introduces no new operations, refusal reasons or record fields: an operation exists only as a row of a sheet, a reason only as a row of `spec/model/reasons.yaml`, a shape only as a schema in `spec/model`.
- A binding does not weaken the guarantees of a sheet: what the sheet forbids (free input on a production machine, write tools in the model catalog, store credentials outside the core), the binding forbids in the same words and says how this is checked in its technology.
- A binding names its technology version (protocol revision, API version, OS version) and the date until which it was checked; the skeleton checks only the named versions.
- Every binding has a suite or fixtures in `conformance/`: a mock technology through which the runner checks the sheet via the binding.
- Tenant data does not appear in a binding: examples use only `demo`, `demo2`, `farm`, the people `alice`, `bob`, `carol`, the machines `web1`, `db1`, `ctd1` and the farm nodes `core1` and `backup1`.
