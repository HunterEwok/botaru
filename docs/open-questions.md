# Open questions

This is what the skeleton deliberately leaves open. The first part concerns the skeleton itself: for each question it records what is assumed by default until there is a decision, and what depends on it. The documents and rules are written to these defaults, so a different decision changes the rules and places named in the row. A question that has been answered moves from here to [decisions.md](decisions.md) as a numbered decision; for the release 0.3.0 every default these tables listed was accepted as D-119, and the tables are empty until a new question comes up. The second part lists what every organization decides when it deploys the farm; the skeleton gives fields and rules for these decisions but does not choose for it.

## Skeleton questions

### Design and code

| Question | Default | What depends on it |
|---|---|---|

### Tasks and delivery

| Question | Default | What depends on it |
|---|---|---|

### Permissions and human decisions

| Question | Default | What depends on it |
|---|---|---|

### Messages, campaigns and scripts

| Question | Default | What depends on it |
|---|---|---|

### Bots, commission and farm manager

| Question | Default | What depends on it |
|---|---|---|

### Improvements

| Question | Default | What depends on it |
|---|---|---|

### Operations, models and money

| Question | Default | What depends on it |
|---|---|---|

### Quality checks

| Question | Default | What depends on it |
|---|---|---|

<!-- open-questions: skeleton -->

## What every installation decides

- **Personal data.** Who the data controller is, which subprocessors, in which region and for how long the model provider keeps requests. The skeleton gives the privacy block in the tenant manifest for this ([operations.md](operations.md#peoples-data)).
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
