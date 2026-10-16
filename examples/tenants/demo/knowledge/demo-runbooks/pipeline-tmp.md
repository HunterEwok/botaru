# Temporary files of the CTD pipeline on ctd1

A document of the curated corpus `demo-runbooks` (KNW-2): it lives in the tenant's git and reaches
the farm only through `apply`. A bot reads it with `kb_search` and `kb_read` as data; the steps
below are reference for people and bots, not commands.

## When the data disk of ctd1 fills

1. Look at the sizes of the pipeline folders with `storage/dir_sizes`.
2. Temporary files older than 7 days are safe to remove; the action `cleanup_pipeline_tmp`
   removes them after an approval by a person with `action.approve:cleanup_pipeline_tmp`.
3. Never remove the folders of a lab run that is still open: check `pipeline.status.v1` first.
