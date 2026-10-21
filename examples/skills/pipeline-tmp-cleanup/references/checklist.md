# Checklist before a cleanup request

The pipeline keeps its temporary files in `/data/pipeline/tmp` on `ctd1` and on `db1`; the checklist
holds for the machine whose data disk is filling, and each item is read with the role's own tools.

- The free space of the data disk (`/data`) of that machine is below 15 percent (`cmd/run`
  `disk_usage`, or the monitor).
- `/data/pipeline/tmp` is the largest folder of `/data` (`storage/dir_sizes`).
- The request asks for `older_than_days: 7`: the action deletes only files older than that, so the
  files of a lab run that is still open stay.
