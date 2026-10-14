# Running the first bot

This guide brings up a farm of version 0.1 of the implementation on one Linux server: the core, one agent of role `hello`, the Telegram channel and the hello-bot example. It runs as a **test bench** (`test_bench: true`): every tenant has `kind: test`, and the model can be the mock provider or a real Anthropic key. A production farm follows once the remaining v0.1 parts are built (README.md, "Not in this version").

Paths below are examples; the installation files (settings, secrets, your own manifests) stay on the server and never go into this repository.

## 1. What is needed

- Linux on amd64 or arm64 with systemd.
- PostgreSQL 16 or later.
- Go, only to build (the `go` command fetches the toolchain named in `impl/go.mod`).
- A checkout of this repository on the server: the core reads its schemas from it at run time.
- A Telegram bot from @BotFather and its token.
- For a real model: an Anthropic API key of a workspace for this tenant.

## 2. Build

```sh
git clone <the address of this repository> /opt/botaru/src
cd /opt/botaru/src/impl
CGO_ENABLED=0 go build -o /opt/botaru/bin/ ./cmd/...
/opt/botaru/bin/botaru-core -version
```

## 3. The store

```sh
sudo -u postgres createuser botaru
sudo -u postgres createdb -O botaru botaru
```

The core creates its tables on start.

## 4. Secrets

Every secret is a file readable only by the user that runs the farm (here `botaru`):

```sh
install -d -m 700 -o botaru /etc/botaru/secrets
cd /etc/botaru/secrets
F=/opt/botaru/bin/farmctl
echo 'postgres:///botaru?host=/var/run/postgresql' > dsn   # the store connection string
$F new-secret > token.key                                 # the core key of token hashes and lease signatures
$F new-secret > operator                                  # your farmctl credential
cat operator | $F hash-token                              # its SHA-256, for the settings below
echo '<the bot token from @BotFather>' > bot
echo '<the Anthropic key>' > anthropic-demo                # only for a real model
chown botaru: * && chmod 600 *
```

## 5. Settings of the core

`/etc/botaru/core.yaml`:

```yaml
spec_root: /opt/botaru/src
store_dsn_file: /etc/botaru/secrets/dsn
token_key_file: /etc/botaru/secrets/token.key
test_bench: true
p1_listen: 127.0.0.1:8401        # workers
p2_listen: 127.0.0.1:8402        # farmctl only; a separate listener (IDN-7)
# test_listen: 127.0.0.1:8403    # optional: the test-control port for the conformance runner
# test_key_sha256: <the SHA-256 of the test bench key>   # needed with test_listen; only with test_bench: true
texts_dir: /etc/botaru/texts     # optional: ru.yaml and other locales of the code texts
operator: "Example operator"     # the operator named in the AI notice (notice.ai)
farm_hosts: [core1]              # the farm nodes (DEP-11): apply refuses a catalog that names them
operators:
  - token_sha256: <the SHA-256 from step 4>
    principal: human:p_01JB00000000000000000000A1   # your id as the operator
    tenant: demo
  # Optional: a credential of tenant farm reaches every tenant and the holds of scope farm.
  # - token_sha256: <the SHA-256 of another token>
  #   principal: human:p_01JB00000000000000000000A1
  #   tenant: farm
providers:
  # The model profile base-std of the example names provider-a. Map it to the mock provider:
  provider-a:
    binding: anthropic
    base_url: http://127.0.0.1:8409
    key_files: {demo: /etc/botaru/secrets/anthropic-demo}   # any text for the mock
```

For a real model, use `base_url: https://api.anthropic.com` (or leave it out) and put the Anthropic key into the key file; step 7 says how to name the model. P4 sets the model, the output limit and the region itself; the runtime has no key.

## 6. Start

Example unit `/etc/systemd/system/botaru-core.service`; the agent, the channel and the mock provider get units of the same shape with their own `ExecStart`.

```ini
[Unit]
Description=botaru core
After=postgresql.service

[Service]
User=botaru
ExecStart=/opt/botaru/bin/botaru-core -config /etc/botaru/core.yaml
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

The mock provider, only for the check without a real model:

```sh
/opt/botaru/bin/botaru-mockmodel -listen 127.0.0.1:8409 -scenario /opt/botaru/src/examples/hello-bot/mock-model.yaml
```

Check the core: `farmctl -token-file /etc/botaru/secrets/operator health` answers `"alive": true`.

## 7. Manifests, the person and the workers

Below `F` is `farmctl -token-file /etc/botaru/secrets/operator`.

```sh
cd /opt/botaru/src
$F apply -as-test -drop-unsupported examples/hello-bot/role.yaml examples/hello-bot/tasktype.yaml examples/hello-bot/group.yaml \
  examples/tenants/demo/tenant.yaml examples/tenants/demo/channels.yaml examples/model-profiles/base-std.yaml
```

`apply` takes the manifests as one set and records the commit of the checkout; `-as-test` applies the tenant with `kind: test`, as the runner does on a test bench. The example tenant and channel carry fields of v0.2 (`locale`, `retention`, `operator_access`, `rate`), which a v0.1 core refuses with `version_unsupported` (MAN-11); `-drop-unsupported` leaves them out and prints a warning for each. It leaves out only whole optional fields; a later kind or a value that gives a field its meaning (a channel direction, a resource kind, a tier) stops the apply with an error naming the path, and you remove that manifest or value yourself. For a real model, copy `examples/model-profiles/base-std.yaml` to your installation folder, set its `model` to an Anthropic model id, and apply your copy in place of the example one, adding `-commit` with the full commit id of the checkout (`git rev-parse HEAD`), because your copy is outside it.

Add yourself with your numeric Telegram id (a bot such as @userinfobot shows it) and the group that may call the greeting:

```sh
$F principal add -identity telegram:<your id> -locale ru     # prints your principal id
$F grant <the principal id> hello
```

Enroll the workers; the tokens are written to files and shown nowhere else:

```sh
$F worker enroll -tenant demo -role hello -kind agent -token-out /etc/botaru/secrets/agent
$F worker enroll -tenant demo -role telegram -kind channel -target telegram -token-out /etc/botaru/secrets/channel
chown botaru: /etc/botaru/secrets/*
```

Start them:

```sh
/opt/botaru/bin/botaru-agent -core http://127.0.0.1:8401 -token-file /etc/botaru/secrets/agent
/opt/botaru/bin/botaru-telegram -core http://127.0.0.1:8401 -token-file /etc/botaru/secrets/channel \
  -bot-token-file /etc/botaru/secrets/bot -link-allowlist www.example.org,docs.example.org -spec-root /opt/botaru/src
```

## 8. The check

Write `/hello hi` to your bot in a private chat. Three messages come back in order:

1. the notice that an AI bot answers (`ai_notice`, first contact only, text `notice.ai` of `spec/bindings/channels/messages/en.yaml`);
2. the privacy notice with its version and link (`privacy_notice`, text `notice.privacy`);
3. `[AI answer]` and the greeting: with the mock provider exactly `Hello, alice! I am the farm bot @demo_bot.`, with a real model a greeting of its own.

Then `farmctl audit verify demo` answers `"ok": true`. A message from someone who is not added gets no answer; `/health` answers "Unknown command." because that type is not applied.

## Tools: a toolhost on a machine

A bot reads a machine through a toolhost on that machine (E3): `botaru-toolhost` takes the tool calls of one tenant over P1 and runs each in a new process of its plugin. This version has three own plugins: `cmd` (named commands such as `disk_usage` and `service_status`), `logs` (the tail of a log file) and `checks` (the latest site check results). Only read tools are called, and only by a role that lists them.

1. **Manifests.** Apply `examples/tools/cmd/tool.yaml`, `examples/tools/logs/tool.yaml` and `examples/tools/checks/tool.yaml`, a `Catalog` of your tenant with the machine and its resources, and a role whose `tools` lists what it may call, for example `[checks/latest, logs/tail, cmd/run]`. The example catalog `examples/tenants/demo/catalog.yaml` carries fields of later versions that cannot be left out; write your own with only `hosts` (with `services` and their `unit`) and `resources` (`list: log_sources` for logs, `list: checks` for checks, each with its `host`). A tool reaches the model only when the task type's `tier` and `data_class` allow it: `logs/tail` and `cmd/run` are tier 1 with output `raw_logs`.
2. **A user per tenant.** The toolhost never runs as root and refuses to. Create one user per tenant on the machine, for example `useradd -r -s /usr/sbin/nologin botaru-tools-demo`, and give it read access only to the log files it serves (for example the `adm` group).
3. **Enroll** it for the machine of the catalog; the record fixes the plugins, the placement and the machine:

   ```sh
   $F worker enroll -tenant demo -role toolhost -kind toolhost -plugins cmd,logs,checks -placement host -host web1 \
     -token-out /etc/botaru/secrets/toolhost-demo
   chown botaru-tools-demo: /etc/botaru/secrets/toolhost-demo
   ```

4. **Settings** `/etc/botaru/toolhost-demo.yaml`: how each plugin starts. Which file a log source is on this machine is set here, never by a call:

   ```yaml
   placement: host
   kill_after: 10s
   plugins:
     - name: cmd
       command: [/opt/botaru/bin/botaru-plugin, cmd, -manifest, /opt/botaru/src/examples/tools/cmd/tool.yaml, -spec-root, /opt/botaru/src, -host, web1]
     - name: logs
       command: [/opt/botaru/bin/botaru-plugin, logs, -manifest, /opt/botaru/src/examples/tools/logs/tool.yaml,
                 -source, nginx-error=/var/log/nginx/error.log, -source, nginx-access=/var/log/nginx/access.log]
     - name: checks
       command: [/opt/botaru/bin/botaru-plugin, checks, -manifest, /opt/botaru/src/examples/tools/checks/tool.yaml, -results, /var/lib/botaru-checks/results.json]
   ```

   The `checks` plugin only reads the results file; a checker of your own writes it, for example `{"www-health": {"ok": 3, "total": 3, "last_status": 200, "ms_max": 180}}`, whole numbers only.
5. **Unit** `/etc/systemd/system/botaru-toolhost-demo.service`:

   ```ini
   [Unit]
   Description=botaru toolhost of tenant demo
   After=network-online.target

   [Service]
   User=botaru-tools-demo
   ExecStart=/opt/botaru/bin/botaru-toolhost -core http://core1.example.org:8401 -token-file /etc/botaru/secrets/toolhost-demo -config /etc/botaru/toolhost-demo.yaml
   Restart=on-failure
   NoNewPrivileges=yes
   CapabilityBoundingSet=
   ProtectSystem=strict
   ProtectHome=yes
   PrivateTmp=yes
   PrivateDevices=yes
   RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

   [Install]
   WantedBy=multi-user.target
   ```

At start the toolhost asks every plugin for its `tools/list` and reports the fingerprint in `hello`. If the manifests in the checkout on the machine differ from those applied to the core, the core moves the toolhost to `blocked`: update the checkout or apply again, then enroll a new toolhost. After an `apply` that changes a plugin manifest, restart the toolhosts of that plugin once their checkout carries the same manifest: until they say hello again they take no calls. A call that no toolhost of the machine takes within 30 seconds ends with `no_executor`, and the model is told so; to stop all tool calls of a tenant at once, set a hold with the effect `no_tool`.

## Stopping work with a hold

A hold stops part of the work at once, without a restart (HLD-3). For example, to stop the greeting type in `demo`, end its running attempts and later lift the hold:

```sh
farmctl hold set tasktype demo hello.greet.v1 -effects no_enqueue,no_claim,cancel_running -reason manual
farmctl hold list
farmctl hold release h_...
```

`-until 2026-10-15T08:00:00Z` ends a hold by itself. To stop the whole farm, also when the core does not answer, run on the core machine `farmctl hold farm -config /etc/botaru/core.yaml -effects no_claim,cancel_running -reason compromised`; it writes the hold straight to the store and needs an operator of `farm` in the settings, or `-by` with your id. Only the operator credential of `farm` lifts it: `farmctl -token-file <farm token> hold release h_...`.

Farm alarms (a silent worker, a task nobody took) come as messages to the private chats of the tenant's admins, people with `admin:people` or `hold.release`. This version cannot grant such a group yet (PRM-13), so the core logs a warning instead of sending.

## Upgrading from the first version

A farm built from the first version of this guide keeps its store, settings and secrets. The steps:

1. Stop the channel, the agent and the core, in this order.
2. Update the checkout and build again:

   ```sh
   cd /opt/botaru/src && git pull
   cd impl && CGO_ENABLED=0 go build -o /opt/botaru/bin/ ./cmd/...
   ```

3. The channel now reads its message texts from the checkout (`spec/bindings/channels/messages/`). It finds the checkout by itself when the programs are in `/opt/botaru/bin` and the checkout in `/opt/botaru/src`. Otherwise add `-spec-root /opt/botaru/src` to the `ExecStart` of its unit, or `Environment=BOTARU_SPEC_ROOT=/opt/botaru/src`, then run `systemctl daemon-reload`. Of the programs, the core (`spec_root` in its settings), the channel and `farmctl apply -drop-unsupported` read the checkout; `farmctl` finds it as a parent of the working directory, or takes `-spec-root`.
4. Add the new `operator` setting to `/etc/botaru/core.yaml` (section 5). It names the operator in the AI notice. Without it the notice says "the operator of this farm", and the core logs a warning at start.
5. Start the core first. It applies the new store migrations by itself at start; they keep the old records and move planned notices to the new text names.
6. Apply the manifests again (section 7) with `-drop-unsupported`, then start the agent and the channel.

## Troubleshooting

- The core does not start with `an address outside the binding list is allowed only on the test bench`: the settings have a mock address while `test_bench` is false.
- `apply` says git could not read the commit, with `dubious ownership`: the checkout belongs to another user than the one running `farmctl`; pass `-commit $(git -C /opt/botaru/src rev-parse HEAD)` run as the owner of the checkout.
- `apply` answers `test_bench_production`: a tenant with `kind: production` on a test bench; add `-as-test`.
- `botaru-telegram` stops with `skeleton root with spec/VERSION not found`: pass `-spec-root` with the checkout, or set `BOTARU_SPEC_ROOT` (see "Upgrading from the first version").
- `apply` answers `version_unsupported` with a path: the manifest has a kind or a field of a later version, or of a profile this core does not claim (MAN-11); remove it, or add `-drop-unsupported` to leave such fields out.
- The agent logs `provider_config`: the provider of the profile has no entry in `providers`, no key for the tenant, or the tenant's `privacy.subprocessors` does not list that provider.
- The toolhost stops with `blocked`: its plugins, placement or live `tools/list` differ from the enroll record or the applied manifests (section "Tools").
- The model gets `error: plugin_unavailable`: no toolhost of that plugin and machine has called the core in the last two minutes.
- Nothing arrives in Telegram: check the channel log; a `403` from the Bot API means the bot was blocked in that chat.
