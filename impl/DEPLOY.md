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
health:                          # optional thresholds of P2 health and its alarms (OBS-9, CAP-5)
  store_volume: /var/lib/postgresql   # the store volume, when the store runs on this machine
  # urgent_age_s: 600, delivery_window_s: 3600, delivery_share_min: 0.9, loop_tick_s: 30,
  # store_free_soft: 0.15, store_free_hard: 0.05
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

A bot reads a machine through a toolhost on that machine (E3): `botaru-toolhost` takes the tool calls of one tenant over P1 and runs each in a new process of its plugin. This version has three own read plugins: `cmd` (named commands such as `disk_usage` and `service_status`), `logs` (the tail of a log file) and `checks` (the latest site check results). Only read tools are called, and only by a role that lists them.

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

## Actions: restarting services and running actions

A bot never writes on a machine with its own tools. A write runs only through the **action toolhost** of the tenant on that machine, under an approval decided on a card or a `service_grant` record (APR-7, APR-18). This version has two write plugins of its own: `service` (start, stop and restart of catalog services through systemd) and `queue` (actions of `Action` manifests with the `oneshot` mechanism: the plugin puts a run request into the tenant queue for a path unit of yours).

1. **Catalog and role.** Declare each controllable service in the catalog: `hosts[].services[]` with `name`, the exact `unit` (for example `nginx.service`), `control` (a subset of `start`, `stop`, `restart`) and `via: host`. `no_card` defaults to `[restart]`: a restart runs without a card, `start` and `stop` always go by card. Give the role `service_control: [nginx]` (or `["*"]`) and apply `examples/tools/service/tool.yaml`.
2. **Rights of people.** The initiator of the task needs `service.control:<service>` in one of their groups, and the person who decides a card `action.approve:service.control` (or `action.approve:service.control:<service>`). A schedule or route task runs as `system`, which gets the right only by an explicit row in a group. These groups are privileged: this version does not give them through `grant`, so add the membership with SQL as for the other privileged groups.
3. **A separate OS user** for the action toolhost, for example `useradd -r -s /usr/sbin/nologin botaru-act-demo`: it is not the user of the read toolhost, is in no group with access to containers or `sudo`, and has no other polkit rules (DEP-9).
4. **The polkit rule** lets this user run exactly the catalog verbs on the exact units of this machine and nothing else. Generate it from the catalog, never by hand:

   ```sh
   /opt/botaru/bin/botaru-plugin polkit -catalog tenants/demo/catalog.yaml -host web1 -user botaru-act-demo \
     > /etc/polkit-1/rules.d/60-botaru-demo.rules
   ```

   Generate it again after every change of the services of this machine in the catalog.
5. **Enroll and settings.** Enroll a second toolhost of the tenant with `-plugins service,fixer` (the plugins of your actions) on the same machine, and give it its own settings file:

   ```yaml
   placement: host
   kill_after: 10s
   plugins:
     - name: service
       command: [/opt/botaru/bin/botaru-plugin, service, -manifest, /opt/botaru/src/examples/tools/service/tool.yaml]
     - name: fixer
       command: [/opt/botaru/bin/botaru-plugin, queue, -manifest, /etc/botaru/tools/fixer.yaml, -queue, /var/lib/botaru/demo/queue]
   ```

   The unit is the one of the read toolhost with `User=botaru-act-demo`. The queue directory `/var/lib/botaru/<tenant>/queue/` is writable only by this user; your root path unit (`PathExistsGlob=/var/lib/botaru/demo/queue/*.json`) starts your request parser, which checks each request against the action schema before it runs anything. The parser is yours: this version does not ship one.
6. **From a chat.** A person with the right writes `/service restart nginx`: the restart runs under a `service_grant` with the person as author and the outcome comes back to the same chat; `/service stop nginx` sends a card to the approvers. `/run <pipeline> <arguments>` starts a pipeline of the tenant's `run` field: an action by card, an action with `risk: routine` without a card within its `runs_per_hour` and `runs_per_day`.

A hold with `no_action` (or `no_tool`) on the tenant, the machine, the plugin `service` or the action `service.control` stops new service actions at once; a hold on the tenant or the action also cancels the pending cards.

## Stopping work with a hold

A hold stops part of the work at once, without a restart (HLD-3). For example, to stop the greeting type in `demo`, end its running attempts and later lift the hold:

```sh
farmctl hold set tasktype demo hello.greet.v1 -effects no_enqueue,no_claim,cancel_running -reason manual
farmctl hold list
farmctl hold release h_...
```

`-until 2026-10-15T08:00:00Z` ends a hold by itself. To stop the whole farm, also when the core does not answer, run on the core machine `farmctl hold farm -config /etc/botaru/core.yaml -effects no_claim,cancel_running -reason compromised`; it writes the hold straight to the store and needs an operator of `farm` in the settings, or `-by` with your id. Only the operator credential of `farm` lifts it: `farmctl -token-file <farm token> hold release h_...`.

Farm alarms (a silent worker, a task nobody took) come as messages to the private chats of the tenant's admins, people with `admin:people` or `hold.release`. This version cannot grant such a group yet (PRM-13), so the core logs a warning instead of sending.

## Backups and restore

The backup is your own job, run as its own user off the core process; the core only checks that it happens. One way, on the core machine every night, with the encryption key of the backup on this machine only:

```sh
pg_dump -Fc botaru | age -r <the backup public key> > /var/backups/botaru/store-$(date -u +%F).age
farmctl erasure-log export -config /etc/botaru/core.yaml > /var/backups/botaru/erasure-$(date -u +%F).json
```

Copy both to the backup machine or to a write-only external store, then report each:

```sh
F="farmctl -token-file /etc/botaru/secrets/backup-operator"   # an operator credential of tenant farm
$F backup report -kind backup -location offsite-1 -started <start, RFC 3339> -finished <end> -ok -size <bytes>
$F backup report -kind backup -location erasure-1 -started ... -finished ... -ok   # the erasure log backup
```

Add to `/etc/botaru/core.yaml`:

```yaml
backup:
  locations: [offsite-1]          # location_id of each store backup place (RET-5)
  erasure_log_location: erasure-1
  monitor: false                  # true when a monitoring system watches the backups
spend:                            # reconciliation with the provider report (BUD-10); these are the defaults
  tolerance_eur: 0.5
  tolerance_pct: 2
  estimate_release_after: 7d
  reconcile_overdue_days: 35
```

Once a week restore the newest backup into a scratch database, check it, and report `-kind restore_check`.

To restore the farm:

1. Stop every farm process. Load the backup into an empty store (`pg_restore -d botaru`).
2. On the core machine: `farmctl restore begin -config /etc/botaru/core.yaml -erasure-log <the newest erasure log backup>`.
3. Start the core. It answers `not_ready` until the restore completes.
4. Reconcile each day from the backup to the day of the restore with the provider's cost report (`farmctl spend reconcile`), then `farmctl restore complete`. The report of the restore day exists only from the next day, so a farm restored the same day usually starts with `farmctl restore complete -spend-pending`: the daily limits are halved until the reconciliation arrives.
5. A worker token revoked after the backup is valid again in the restored store. Rotate the token key without keeping the old one (section "Rotating the token key", with no `previous_token_keys`), enroll every worker again, and start them.

## Rotating the token key

```sh
farmctl new-secret > /etc/botaru/secrets/token-k2.key
```

In `/etc/botaru/core.yaml` point `token_key_file` at the new file, set `token_key_id: k2`, and keep the old key for the window:

```yaml
previous_token_keys:
  - {id: k1, file: /etc/botaru/secrets/token.key}
```

Restart the core. After the window (every worker has called once, and the longest lease has passed) remove the old key from `previous_token_keys` and restart again.

The signing key of audit heads is derived from the token key. Its public key is kept in the table `audit_head_keys` when it first signs, so heads in the monitoring history stay verifiable after the old token key is removed. Always give a new token key a new `token_key_id`: under an id the store already knows with another key the core signs no head, and `doctor` fails the line `monitor.head_key_id`. A verifier outside the farm can take the public keys from there:

```sh
psql -c "SELECT key_id, encode(public_key, 'base64') FROM audit_head_keys"
```

## Running with docker compose

`impl/deploy/compose/` runs the same test bench in containers: the store, the core, the agent, the Telegram channel and the mock provider. The containers run as user 10001 with a read-only root, and only the store and core networks connect them; P1 and P2 publish no port.

```sh
cd /opt/botaru/src/impl/deploy/compose
install -d -m 700 secrets && cd secrets
F="docker run --rm botaru:local farmctl"
docker compose -f ../compose.yaml build core
$F new-secret > store-password
printf 'postgres://botaru:%s@store:5432/botaru?sslmode=disable' "$(cat store-password)" > store-dsn
$F new-secret > token-key
$F new-secret > operator
cat operator | docker run --rm -i botaru:local farmctl hash-token    # put it into ../core.yaml
echo '<the bot token from @BotFather>' > bot-token
echo '<the Anthropic key, any text for the mock>' > provider-key-demo
chmod 600 * && sudo chown 10001 * && sudo chown 999 store-password
cd ..
```

Then start the store, the core and the mock provider, apply the manifests and enroll the workers from inside the core container, and start the workers. The image has no git history, so `apply` takes the commit of the checkout the image was built from:

```sh
docker compose --profile mock up -d store core mockmodel
X="docker compose exec core farmctl -token-file /run/secrets/operator"
$X apply -as-test -drop-unsupported -commit $(git -C /opt/botaru/src rev-parse HEAD) examples/hello-bot/role.yaml examples/hello-bot/tasktype.yaml examples/hello-bot/group.yaml \
  examples/tenants/demo/tenant.yaml examples/tenants/demo/channels.yaml examples/model-profiles/base-std.yaml
$X principal add -identity telegram:<your id> -locale ru
$X grant <the principal id> hello
$X worker enroll -tenant demo -role hello -kind agent -token-out /var/lib/botaru/workers/agent
$X worker enroll -tenant demo -role telegram -kind channel -target telegram -token-out /var/lib/botaru/workers/channel
docker compose --profile mock --profile workers up -d
```

The check is the same as section 8. For a real model, set `base_url` in `core.yaml` to `https://api.anthropic.com`, leave the `mock` profile out, and apply your copy of the model profile as in section 7 (mount it into the core container).

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
