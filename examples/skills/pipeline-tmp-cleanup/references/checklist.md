# Checklist before a cleanup request

The pipeline keeps its temporary files in `/data/pipeline/tmp` on `ctd1` and on `db1`; the checklist
holds for the machine whose data disk is filling.

- The free space of the data disk (`/data`) of that machine is below 15 percent.
- The temporary folder `/data/pipeline/tmp` holds files older than 7 days.
- No lab run is open on that machine.
