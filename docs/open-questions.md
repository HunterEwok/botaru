# Open questions

This is what the skeleton deliberately leaves open. The first part concerns the skeleton itself: for each question it records what is assumed by default until there is a decision, and what depends on it. The documents and rules are already written to these defaults, so a different decision changes the rules and places named in the row. A question that has been answered moves from here to [decisions.md](decisions.md) as a numbered decision. The second part lists what every organization decides when it deploys the farm; the skeleton gives fields and rules for these decisions but does not choose for it.

## Skeleton questions

### Design and code

| Question | Default | What depends on it |
|---|---|---|

### Tasks and delivery

| Question | Default | What depends on it |
|---|---|---|
| Whether to repeat the mark "you are talking to a bot" after a long break in the conversation | No: the mark goes on first contact in a channel and into every newly bound chat (DLV-12). | How often people see the mark. |
| Date format in task type parameters (for example the field `date` of `report.sensor.v1` in flow 5) | None: the registry `spec/model/formats.yaml` has no date format, only `datetime` and `year_month`; `examples/tasktypes/report.sensor.v1.yaml` has no `date` field. | Whether a task type can accept a date without a time in a closed format. |
| How a schedule sets the parameter "last month" | It cannot: the text gives no way to set a parameter relative to the slot in `Schedule.spec.task`, so the schedule `month-report-archive` from flow 7 is not declared in `examples/tenants/demo/schedules.yaml`. | Whether monthly reports can be enqueued by a schedule without a person. |
| Whether to check a task for stalling if the type did not set `stall_after` | No: without the field in the task type there is no stall check (CNT-4). | When people learn about a task that has not moved for a long time. |
| Self-stop margin of a lease holder | 10 seconds before the end of the lease, then another 10 seconds until the forced stop (`kill_after`) (FEN-6). | How much work is lost when a lease is lost and how quickly the resource is freed. |
| Number of delivery attempts | 5 attempts, a pause from 10 seconds doubling up to 10 minutes; the channel binding can lower the number (DLV-4). | When a delivery counts as dead and reaches the operator. |

### Permissions and human decisions

| Question | Default | What depends on it |
|---|---|---|
| Whether writing to a knowledge corpus (`kb.write`) is privileged | No: the corpus class is checked with the permission `data:<class>` (KNW-4). | Who adds to the tenant's knowledge corpora. |
| Card deadlines and limits: 14 days, 7 days, 1 hour, 30 minutes, 10 cards per day per person, a reading threshold of 10 seconds | The values from the documents; they are refined by the experience of the first tenants. | The load on people and the speed of decisions. |
| Whether to reject tasks outside the declared data processing purposes | No: only the purpose `incident_review` changes the farm's behavior (SEC-8). | How strictly the privacy block limits work. |
| Whether a restart needs a card if the task saw external logs rather than the monitoring system | Yes, a card (D-70, D-82). | How many log-based restarts wait for a person. |

### Messages, campaigns and scripts

| Question | Default | What depends on it |
|---|---|---|
| Campaign queue limit `queue_max` without a value | An item waits in `pending` while the tenant queue is below `max_queued`, and gets no refusal (ORC-4). | The behavior of large campaigns under load. |
| Checking the numbers of the reporter's paragraph against the table | A SHOULD-level rule since v0.2; the checking algorithm is chosen before v0.2 (REP-4). | How strictly the numbers in summaries are checked. |
| Decision deadline for a script `confirm` step | 30 minutes and no longer than the run deadline; silence gives `declined` (SCR-6). | How long a script waits for a person. |
| How code learns that an action is named in the text of an assignment | A match of the action name or its `title` after simple normalization; there are no synonyms in the manifest. | How precisely the translator needs to name an action. |
| Whether to merge `propose_campaign` and `propose_script` | No; the `fanout` format is compatible, and the question is decided in v0.2. | One or two paths to propose mass work. |
| Starting a script from git on a schedule | Since v0.2, only without steps with an action and below the tenant's thresholds. | Whether regular scripts can be automated. |
| An assignment to the translator from an external program (principal `service`) | A script draft is possible, but nobody can start it, because only a person confirms a run. | Whether programs can order scripts. |

### Bots, commission and farm manager

| Question | Default | What depends on it |
|---|---|---|
| Rank ceiling of a personal assistant | The ceiling of the `worker` profile, as for a functionary (IDN-5). | Which model a personal assistant can get. |
| Whether a reason code is needed to read the sealed memory of a retired bot | No: an audit record with the author and the time is written (MEM-4). | How thoroughly reading sealed memory is explained. |
| Whether to close automatically resolved incidents that nobody closed | No: they wait for a person or a closing pass. | How many incidents stay in the resolved state. |

### Improvements

| Question | Default | What depends on it |
|---|---|---|
| Whether proposals without a change (kind `none`) go through the full commission | Yes, by the same path and at the same cost. | The cost of reviews and the load on the commission. |
| Review wait limit `review_wait_max` | 7 days (COM-6). | When a proposal without reviews moves on. |
| Tolerances and minimum samples of core series | The values of the series registry table; a tenant does not change them (OBV-6). | How early a change trial gives an outcome. |

### Operations, models and money

| Question | Default | What depends on it |
|---|---|---|
| Which provider answers mean the workspace spend ceiling | The provider binding decides; the rules contain no response codes. | How the core tells an exhausted limit from provider overload. |
| The provider's usage report after a restore from backup is not ready yet | The core waits for the report and does not become ready (RET-6). | How long the farm stands still after a restore. |
| A return window after a person leaves | None: turning off with `/off` is already reversible, and leaving is final. | Whether a person's data can be returned after they leave. |
| A separate retention class for usage summaries without people | No, usage is stored as `metadata`. | How long the usage history is kept. |
| How many consecutive schedule slots must be lost before the schedule is paused | 3 slots (CAP-4). | How quickly the farm stops a broken schedule. |
| How the monitoring system tells a planned core deployment from a failure | For the deployment a person sets a maintenance window on the farm nodes in the monitoring system (MON-2). | Whether there will be false alarms during a deployment. |
| The period after which an unclosed erasure request gives an alarm | Set by the deployment binding. | How quickly people learn about a stuck erasure. |

### Quality checks

| Question | Default | What depends on it |
|---|---|---|
| Drift thresholds of bot checks | 0.2 of the mean score, 5 percentage points of the veto share, 0.05 of the share of unjustified requests. | When a change of model or runtime counts as a degradation. |
| Reference check of the answers of science roles | Later, after cases with GeoJSON and NetCDF data. | When science roles get their own eval suite. |

<!-- open-questions: skeleton -->

## What every installation decides

- **Personal data.** Who the data controller is, which subprocessors, in which region and for how long the model provider keeps requests. The skeleton gives the privacy block in the tenant manifest for this ([privacy.md](privacy.md)).
- **Retention periods.** Until the installation decides (D-12), the defaults apply: content 30 days (sessions without memory 7 days), metadata 400 days, memory until an erasure request ([operations.md](operations.md)).
- **Model provider.** Who accepts the provider terms, in whose name the keys are issued, what spend ceilings each workspace has (D-42); whether an embeddings provider is needed for knowledge corpora (by default corpora are searched by words) and batch mode for non-urgent tasks (not used by default).
- **Machines.** The core server and its OS, which machines host the tool hosts, which machines the tenant catalog links to monitoring system hosts (`monitor_host`), which services on them bots may start, stop and restart (`services[]`), and for which roles.
- **Storage.** Where the organization's data lives and whether zero retention at the provider is turned on.
- **People and quotas.** The circle of users, their groups and quotas.
- **Actions.** Which actions the organization lets bots request and who approves them; by default the action catalog is empty.
- **Channels.** Which channels are connected (Telegram, email, other messengers) and who owns the bots in the messenger.
- **Moving.** Whether the conversation history moves when the farm moves to another installation.
- **Names.** The names of bots and tenants.
- **Quality checks.** The budget for bot quality checks (eval) and the key limits for them (by default a separate workspace with a monthly ceiling), the test bench machine (by default a separate Linux machine outside the production farm), branch protection in the code hosting, and who brings real questions from colleagues for cases (by default the cases are invented and are added to from failure reviews).
- **Scenarios.** The real tasks the installation starts with.
