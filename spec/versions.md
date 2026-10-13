# Versions and compatibility

## In short

The skeleton has several numbers, and each has its own meaning. `spec/VERSION` is the release number of the repository. The skeleton versions v0.1 and v0.2 are content slices: from which release a rule, an operation or a concept is part of the skeleton and is checked. The major numbers of contracts (the `/v1` path of the HTTP binding, the `.vN` suffix of types and events, `:N` in the `$id` of schemas, the major P1 version in `hello`) are a compatibility promise. Port sheets carry their own SemVer version. This page collects the map of all numbers and the one promise the skeleton keeps.

## Compatibility promise

Within a major version a contract changes only by addition. This is rule MAN-3, and it applies already in the 0.y.z releases, not only after 1.0.0:

- a schema `<name>.schema.json` with `$id` `urn:botaru:schema:<name>:1`, a task type, an event type and a message type `.vN` change only by adding optional fields and new values where the receiver tolerates them; a breaking change is a new file `<name>.v2.schema.json` or a new type `.v(N+1)` next to the previous one (the check `ci/check-additive`);
- the `/v1` path of the HTTP binding changes only by adding operations, optional fields and headers; a breaking change is the path `/v2`, and the core keeps a window of the current and the previous major P1 version (LCY-7);
- a value of a closed registry (`spec/model/*.yaml`) is never removed and never changes its meaning; a retired value stays as a marked entry;
- a rule number is never reused; a retired rule stays as a `retired` row in `conformance/requirements.yaml`;
- an operation of a port sheet is never removed and never changes its meaning within a major version of the sheet; a new operation is a minor version of the sheet.

What the promise does not cover and may change in any 0.y.z release, with an entry in `CHANGELOG.md`:

- the text of the `docs/` documents, and the prose of sheets and bindings if it does not change a contract;
- the fields of manifests with `apiVersion: botaru/v1alpha1`: this is an alpha version until release 1.0.0 (D-44), the changes are visible in the changelog, and `botaru/v1` after 1.0.0 follows the same addition rule;
- fixtures, check suites and eval cases.

The promise applies from the first published release 0.1.0. Before that tag (the "Unreleased" section of the changelog) the forms are still being assembled, and changes of forms are made without new major files.

## Number map

| Number | Where it is written | Meaning | How it grows |
|---|---|---|---|
| release version | `spec/VERSION`, a git tag | the SemVer of the repository 0.y.z | the minor digit when the next skeleton version comes out, the patch for fixes |
| skeleton version | the `version` field of a rule, `since` of a registry entry, `x-botaru-version` of a schema, the "Version" column of a sheet | from which release a rule or an operation is checked: v0.1 means the releases 0.1.z, v0.2 means 0.2.z | a new content slice |
| port sheet version | the line "Sheet version" at the start of every sheet `spec/ports/*.md` | the SemVer of the sheet | minor for a new operation, patch for a clarification of the text, major only together with a new major version of the binding |
| major P1 version | the `/v1` path of the binding `spec/bindings/http/`, the value `v1` in `protocol_versions` of the `hello` request | the worker protocol | a new major only for a breaking change; a window of the current and the previous one (LCY-7) |
| major schema version | an `$id` of the form `urn:botaru:schema:<name>:<major>`, the file `<name>.v<major>.schema.json` from the second one on | the shape of data | only for a breaking change (MAN-3) |
| `.vN` suffix | the name of a task type, an event, a message type, an alarm in the catalog | the meaning and the data shape of that type | `.v(N+1)` next to `.vN` |
| manifest `apiVersion` | the manifest wrapper | `botaru/v1alpha1` until 1.0.0, then `botaru/v1` (D-44) | once, at release 1.0.0 |
| tool protocol profile | `server/discover` of P3, `spec/bindings/mcp/README.md` | the MCP revision and the profile `botaru-tool-gateway/1` | a new profile in the binding |
| provider binding version | `spec/bindings/model-providers/<provider>.md` | the provider's API version and the date it was checked | a new provider API version is a change of the binding |
| registry `version` | the `version` field of the file `spec/model/<registry>.yaml` | the revision number of the registry | grows with every change |
| `conforms_to` | the P2 `status` response | which skeleton version and which conformance profiles the implementation claims (MAN-8) | a claim of the implementation |

## Port sheet versions

| Sheet | Sheet version |
|---|---|
| `p1-worker-protocol.md` | 0.1.0 |
| `p2-admin-api.md` | 0.1.0 |
| `p3-tool-gateway.md` | 0.1.0 |
| `p4-model-gateway.md` | 0.1.0 |
| `c1-task-store.md` | 0.1.0 |
| `c2-authorizer.md` | 0.1.0 |
| `c3-session-store.md` | 0.1.0 |
| `c4-memory-store.md` | 0.1.0 |
| `e1-channel.md` | 0.1.0 |
| `e2-sink.md` | 0.1.0 |
| `e3-tool-host.md` | 0.1.0 |
| `e4-agent-runtime.md` | 0.1.0 |
| `e5-monitor.md` | 0.1.0 |
| `test-control.md` | 0.1.0 |
| `background-loops.md` | 0.1.0 |

The v0.2 operations are already written in the sheets as rows with version v0.2; when 0.2.0 comes out, the sheets get the minor version 0.2.0. The machine map of the operations and their schemas lives in `spec/ports/operations.yaml`, and every row there has a `since` field.
