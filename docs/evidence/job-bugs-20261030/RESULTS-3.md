# A burst on a fresh outbox loses no job: live proof, 2026-10-30

"Before" is unmodified 83487e3d, which is v0.1.248. Every job passed `--state-dir`, each round on a
new outbox. Mac rigs ran under `.dev/`; the Linux one ran under `/tmp` on duklerver at `nice 19`, as
plain processes with no daemon. No real daemon, `/data/workass`, `/data/ml` or GPU was touched and no
update was applied: the real daemon kept pid 179929 and its 4 jobs. Output is in `outbox-14k.log`,
commands in `harness/14k/`. No fault was injected.

## Cause

The outbox is the one SQLite file several processes create together, and opening it read two things
apart that only mean something together. Reproducing it found three refusals, not the one reported:

1. **`recorded migration 1, want 1 of 0`.** `migrate` read `user_version`, then the history. Another
   supervisor's first migration committed between the reads.
2. **`database has tables but no migration history`.** Inside `verifyHistory`: no `schema_migrations`
   table, then a count of tables, with the same commit between them.
3. **`state database is locked by another process`.** `PRAGMA journal_mode = WAL` on the new file can
   answer SQLITE_BUSY at once, without waiting out `busy_timeout`, and that happens at connect.

## Fix

The version and the history are read in one transaction, which takes the write lock, so no commit can
fall between them. On a `Shared` database a pragma that answers busy is retried for up to the busy
timeout. `state.db` does not get that wait: a second daemon still fails fast.

## Results

Refused jobs never ran.

| Case | Before (v0.1.248) | After |
|------|-------------------|-------|
| Mac, CLI, 24 at once, 1400 rounds | **49 of 33,600** jobs, 36 rounds | 0 of 33,600 |
| Mac, CLI, 96 at once, 100 rounds | **9 of 9,600** | 0 of 9,600 |
| Mac, reported to a dev daemon with a real renderer, 24 at once, 40 rounds | **3 of 960**, 2 rounds; those 3 have no row | 0 of 960, 960 rows `exited` |
| duklerver (tmpfs), CLI, 24 at once, 300 rounds | **84 of 7,200** | 0 of 7,200 |

Before, by refusal, on the Mac CLI rows then Linux:

| Refusal | Mac | Linux |
|---------|-----|-------|
| recorded migration 1, want 1 of 0 | 32 | 42 |
| database has tables but no migration history | 12 | 26 |
| locked, `PRAGMA journal_mode = WAL` | 14 | 16 |

Each shape's after count is 0, so none of the three is hiding behind the others. The two Mac rows of
1000 rounds and the daemon rows shared the machine with each other, in both directions.

`TestSharedOpenSurvivesACreatorBetweenVersionAndHistory` drives refusal 1 and fails without the fix.
`TestPragmaWaitsOutBusyOnlyWhenAskedTo` covers the retry with a fake connection, because no holder I
could set up in a test made SQLite answer busy at once; refusal 3 is proven by the rates above.

## Not proven

- Refusal 2 has no deterministic test: it sits between two reads inside the transaction, and only the
  rates show it gone.
- duklerver's rig ran on tmpfs, so a real Linux disk's fsync timing was not exercised. The Mac rigs
  were on a real disk. Reported jobs were not run on Linux.
- Windows: the same code, never run.
- Opening an existing outbox now takes the write lock briefly. The rigs had no open waiting on a long
  transaction, and an outbox writes only short ones.
- A daemon reading the outbox of a dead supervisor during a burst of first opens was not staged. The
  daemon rig deleted the file between rounds with the daemon holding it open at most once per run, so
  each round's outbox was new to the supervisors but not always to the daemon.
