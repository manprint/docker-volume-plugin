# dvol — full review (2026-08-08)

Scope: `pkg/dvol` (1624+295+331+245+127+231 LoC), `cmd/dvol-cli-plugin` (793+86),
all Go tests (2 800 LoC), the three acceptance suites, README/Specs.

Baseline: `go vet ./...` clean, `go build ./...` clean, `go test ./...` PASS.
Every finding below marked *(verified)* was reproduced with a throwaway probe
test in-tree; the probes were deleted afterwards (`git status` clean).

---

## Blockers — silent data loss or unrestorable backups

### B1. A bundle archive restored through the single-volume path silently destroys the target *(verified)*

`Store.RestoreSource` (`pkg/dvol/backup.go:832`) and `Store.RestoreSourceReader`
(`:876`) never check what kind of archive they were handed. A compose/bundle
archive extracts cleanly into staging and `replaceStage` swaps it in.

Probe result — volume `web` contained `real.txt`:

```
restore bundle-shaped archive as single volume err=<nil>
volume web now contains: [. app app/data.txt db db/data.txt manifest.json]
```

No error, no warning, original content gone. Reachable from the CLI:

```bash
docker dvol restore app ~/.local/share/docker-volume-backup/backups/compose/demo-….tar.gz
rclone cat remote:bundle.tar.zst | docker dvol restore --stdin app
```

`ResolveBackup` (`:1599`) happily resolves any path or glob, so this is one
tab-completion away. The opposite direction *is* guarded (a single-volume
archive passed to `--bundle` fails in `splitBundlePath`), which shows the
asymmetry is an oversight, not a design choice.

Fix: after opening the archive, if the first entry is `manifest.json` with
`format == "dvol-bundle-v1"`, refuse and point at `restore --bundle`. The
manifest is currently parsed in exactly one of the five restore entry points
(`RestoreBundleSourcesReader`, `:1179`).

### B2. Any file that changes size during the read aborts the whole backup *(verified)*

`addTreeToWithProgress` (`:743-766`) writes the tar header from the `fstat`
taken at open time, then `io.Copy`s the file. Both directions fail hard:

```
backup of growing file   err=archive/tar: write too long
backup of shrinking file err=archive/tar: missed writing 32505856 bytes
```

This is the normal state of a live volume (WAL, logs, mongod journal). The
README claims the opposite (`README.md:484-486`): *"se un container scrive
durante la lettura, l'archivio può contenere dati provenienti da istanti
diversi"* — in reality nothing is produced at all.

No test covers it: every suite quiesces first (`acceptance_e2e.sh:227` and
`:301` stop the container, `acceptance_mongo_e2e.sh:415-416` does
`fsyncLock` + `docker pause`).

Fix: `io.CopyN(tw, f, hdr.Size)`; on short read zero-pad the remainder, on
excess discard the tail, and log a `WARN file changed as we read it` per entry
(GNU tar semantics). Optionally add a `--fail-on-change` flag for those who
want the current strictness.

### B3. A FIFO in the volume produces an archive that can never be restored *(verified)*

Backup writes a `TypeFifo` header (`:685` default branch → `tar.FileInfoHeader`).
Restore skips creating the node (`extractEntryWithProgress` default branch,
`:1525`) but `extractTarReaderWithProgress` appends a `fileMeta` for it anyway
(`:1359`), so the metadata pass chowns a path that does not exist:

```
backup  err=<nil>
restore err=chown …/.restore-940759005/run.sock: no such file or directory
```

The backup looks successful and is unrestorable — the worst possible failure
mode for this tool. Same for character/block devices.

Fix: have `extractEntry` report whether it materialised the entry and only
record `fileMeta` for materialised paths; or create the node with `mknod` when
running as root.

### B4. A unix socket in the volume aborts the backup *(verified)*

```
backup err=archive/tar: sockets not supported
```

`tar.FileInfoHeader` refuses sockets, and the error propagates out of
`addTreeToWithProgress` (`:743`). MySQL's `mysql.sock` in the datadir, or any
app that keeps its control socket inside its data volume, makes the volume
un-backupable with no workaround.

Fix: skip sockets (and unsupported types) with a `WARN`, exactly as tar does.

---

## High

### H1. `docker dvol restore volA volB` breaks precisely in the disaster-recovery case *(verified)*

`cmd/dvol-cli-plugin/main.go:445-450` decides whether the last argument is a
volume or a backup selector by asking Docker whether that volume *currently
exists*. After losing a volume — the reason you are restoring — it does not:

```
$ docker dvol restore web db      # db was deleted
docker dvol: no backup matching "db" for volume "web"   (exit 1)
```

`README.md:344-346` documents `docker dvol restore app db`, and `:59-61`
promises missing volumes are created on restore. Both are false in this path.

Fix: disambiguate on "does this argument resolve to an existing archive"
instead, or add an explicit `--archive <path>` flag and stop guessing.

### H2. `compose-restore` cannot find bundles created with `--timestamp` *(verified)*

`latestBundle` (`main.go:763`) searches for `project + "-"`, but
`archiveFilename` (`backup.go:428-440`) joins the timestamp with `_`:

```
plain   bundle = demo-20260808-075843.694163967.tar.gz   → found
stamped bundle = demo_08082026_101010.tar.gz             → not found
latestBundle → "" err=no compose backup found for project "demo"
```

`--archive-name` makes bundles unfindable for the same reason. Fix: match
`project` followed by `[-_]`, or give bundles the same persistent index the
flat single-volume backups already have.

### H3. compose backup/restore disagree on the volume set, and the failure has a side effect *(verified)*

`existingVolumes` (`main.go:731`) silently skips declared volumes that don't
exist in Docker. `cmdComposeRestore` (`main.go:592`) then creates *every*
declared volume before any validation, and only afterwards does the manifest
check reject the mismatch:

```
compose-backup selects [proj_data] (declared: 2)
compose-restore targets=2
RestoreBundleSources       err=bundle contains no volume "proj_cache"
RestoreBundleSourcesReader err=bundle volume list does not match restore targets
stray volume proj_cache created by the failed restore: true
```

A failed restore leaves new empty Docker volumes behind. Fix: resolve the
manifest ↔ target-list check before touching Docker; and make compose-backup
loud (or failing) when a declared volume is missing.

### H4. One bad entry in `.dvol-index.json` blocks listing *and* new backups *(verified)*

`loadFlatIndex` (`backup_index.go:67-69`) returns an error for the entire index
on the first invalid entry:

```
poisoned index: infos=0 err=invalid flat backup metadata entry "x.tar.gz"
subsequent backup   err=invalid flat backup metadata entry "x.tar.gz"
```

Corrupted metadata must never stop you from taking a *new* backup. Fix:
drop/quarantine the offending entry with a `WARN`, keep the rest, rewrite.

### H5. Nothing prevents restoring into a volume a running container has mounted

`replaceStage` (`:1250`) renames the mountpoint directory aside and
`RemoveAll`s it. A running container keeps the old inode: it goes on writing
into an unlinked directory and never sees the restored data. The repo is aware
of this — `acceptance_stream_e2e.sh:213-214` explicitly comments it and the
suites always stop the containers first — but the code does not enforce it and
the README only mentions it in prose (`:470`).

Fix: `docker ps --filter volume=<name> -q`; refuse unless `--force`.

### H6. Interrupted-restore recovery never runs for Docker volumes — i.e. for every real use

`recoverInterruptedReplacements` (`store.go:66`) is driven by the entries of
`<root>/meta/*.json` and scans `<root>/volumes/` for directories prefixed
`.<volume>.dvol-old-`. The CLI never restores into `<root>/volumes`: it restores
into the Docker mountpoint, so `replaceStage` (`backup.go:1268`) parks the old
copy at

```
/var/lib/docker/volumes/<vol>/._data.dvol-old-XXXXXX
```

Two independent mismatches: the directory is never scanned, and the prefix would
be `._data.dvol-old-` (basename of the target) rather than `.<vol>.dvol-old-`.

Consequence: if the process dies in the one-rename window inside `replaceStage`,
a Docker volume is left with **no `_data` directory at all** and its previous
contents parked in a hidden sibling that nothing will ever recover — while
`README.md:521-523` promises *"le sostituzioni interrotte vengono recuperate
all'avvio successivo"*. The recovery code is exercised only by
`dvol_test.go:19` (`TestStoreRecoversInterruptedReplace`), which uses the
store-managed layout, so the tests confirm a path production never takes.

Fix: make recovery scan the parent directory of each restore target (or record
in-flight swaps in a small journal under the repository root, which also covers
the case where the process dies before it can be re-run at all).

---

## Medium

| # | Finding | Where |
|---|---------|-------|
| M1 | `O_NOATIME` is unconditional → `EPERM` for any file the caller does not own *(verified: opening `/etc/hosts` as uid 1000 fails)*, plus `EINVAL/EPERM` on NFS and some FUSE. Documented as deliberate (`README:477-480`) but with no escape hatch, those volumes simply cannot be backed up. | `backup.go:540` |
| M2 | `readlinkAt` restores symlink timestamps with `UtimesNanoAt` — a write to the source. On a read-only source filesystem the backup fails; strictly the backup is not read-only at syscall level. | `backup.go:606` |
| M3 | Bundle name collisions surface as a raw `link …: file exists`; bundles support neither `--overwrite` nor `--same-destination`, unlike single-volume backups. | `backup.go:345`, `main.go:328` |
| M4 | `RestoreBundleSources` re-opens and re-decompresses the whole archive once per volume, inside the per-name loop. A 10-volume bundle is decompressed 10 times. | `backup.go:1058` |
| M5 | Restore buffers one `fileMeta` per entry until EOF; backup does `Readdir(-1)` per directory. Volumes with tens of millions of files need GBs of RAM. | `backup.go:1333`, `:641` |
| M6 | `BackupBundleSourcesWithOptions` never validates `prefix` — `"../../escaped"` writes outside `outDir` *(verified)*. Not reachable from today's CLI (project names normalised, `--archive-name` validated) but it is an exported API. | `backup.go:213` |
| M7 | xattrs, POSIX ACLs and file capabilities are silently dropped (plain `tar.FileInfoHeader`). Not mentioned anywhere in the docs. | `backup.go:743` |
| M8 | The flat-index flock is held for the entire archive write, so `docker dvol backups` blocks for the whole duration of a concurrent flat backup. | `backup_index.go:248` |
| M9 | No cross-process lock around restore. Another process running `NewStore` executes `recoverInterruptedReplacements` and can rename the rollback directory back while a swap is in flight → `replace volume: … (rollback failed: …)`. Narrow window, store-managed layout only. | `store.go:66` |
| M10 | Sparse files are expanded to full size on both backup and restore. | — |

---

## Low / polish

- **L1** Archives and index are `0644` (`backup.go:320`, `backup_index.go:217`) — world-readable backups of potentially sensitive volumes. Documented (`README:337`), but `0600` is the safer default.
- **L2** `lz4CompressionLevel` (`compression.go:144-152`): `level <= 0` is remapped to 6 first, so `return lz4.Fast` is dead code and the doc comment "0 is the fast preset" (`compression.go:35`) is wrong.
- **L3** `latestBundle` allocates `(&dvol.Store{})` to call a method that ignores its receiver (`main.go:759`). `ListArchives` should be a package function.
- **L4** `NewStore` always creates `volumes/` and `meta/` (`store.go:34`) although the CLI plugin never keeps volume data there.
- **L5** `extractGlobalFlags` / `hasGlobalDestination` (`main.go:117`, `:157`) strip global flags anywhere in argv, including after `--` and where the token is another flag's value.
- **L6** `restoreVolume` would nil-deref if `a.docker` were nil: `volumeSource` returns `errDockerVolumeNotFound` for a nil client (`main.go:171`) and the caller then calls `a.docker.Create` (`:187`).
- **L7** Double `Close()` on the decompressor: `extractTarToWithProgress` defers `r.Close()` and `finishArchiveReader` closes it too (`backup.go:1300`, `:1318`).
- **L8** `usage()` mixes tabs and spaces (`main.go:633`, `:635`) and omits `--overwrite`/`--bundle` from the restore synopsis.
- **L9** No retention/prune: nothing removes old archives or index entries, no `dvol rm-backup`.
- **L10** No integrity sidecar and no `verify` command. Corruption *is* caught by the compression trailers — verified for all four formats, three flipped bytes each, target left intact — but an archive can only be validated by attempting a full restore.

---

## Architecture

1. **`Store` has two unrelated jobs.** It is both an archive repository and a
   manager of dvol-owned volumes (`volumes/`, `meta/`, `Create/Get/List/Remove`,
   `recoverInterruptedReplacements`). The CLI only ever uses the first role; the
   second exists solely for tests. Splitting `ArchiveRepo` from `VolumeStore`
   would remove `store.go:34`'s unused directories, the `(&dvol.Store{})` hack
   and half the API surface.

2. **Four backup entry points and five restore entry points**, each re-doing its
   own validation:
   `Backup*` / `BackupSource*` / `BackupSourceToWriter` / `BackupBundle*` (×3),
   `Restore` / `RestoreSource` / `RestoreSourceReader` / `RestoreBundle` /
   `RestoreBundleSources` / `RestoreBundleSourcesReader`.
   The guarantees differ between them — the manifest is validated in exactly one
   — which is the direct cause of B1. A single `restore(archiveSource, layout,
   targets)` with the validations factored out would collapse the matrix.

3. **The read-only-source invariant is genuinely well built.** `openReadOnlyDir`
   / `openReadOnlyAt` with `O_NOFOLLOW|O_NOATIME|O_DIRECTORY` plus `SameFile`
   re-verification, the symlink-timestamp restoration, `ensureBackupDestination
   OutsideSources` with partial-path symlink resolution, publish-by-`Link`,
   `syncDir`, the flat-index pending/recover protocol and `replaceStage`'s
   keep-the-old-copy rollback are all careful, correct work, and the
   source-invariance suite pins them well. The defects above are all in the
   *other* direction: what happens to the archive and to the target.

---

## Test-suite gaps

The existing suites are strong on adversarial archives (zip-slip, symlink
escape, symlink volume root, hardlink escape, chmod-through-symlink, truncation,
crash-during-publish, concurrent backups). What is missing maps 1:1 onto the
findings:

1. No backup of a volume that is being written — the exact case B2 breaks. Every
   suite quiesces first.
2. No FIFO / socket / device node / xattr / ACL / sparse-file fixtures (B3, B4, M7, M10).
3. No restore into a volume mounted by a running container (H5).
4. No cross-kind restore: bundle → single-volume, single-volume → `--bundle` (B1).
5. No `restore volA volB` with one target missing (H1); no `compose-restore`
   without an archive argument after `--timestamp` (H2); no compose volume-set
   mismatch (H3); no corrupted-index case (H4).
6. CLI tests call `cmdBackup`/`cmdRestore`, which call `fail()` → `os.Exit(1)`
   (`main.go:789`). A regression kills the test binary instead of failing a
   test. Inject an error sink so CLI-level behaviour can actually be asserted.
7. `TestTimestampCollision` (`backup_test.go:555`) asserts essentially nothing —
   it only `t.Logf`s when the two timestamps are equal.
8. `mongo-compose.yaml` declares bind-backed local volumes
   (`driver_opts: type=none, o=bind`), but after `docker volume rm` a dvol
   restore recreates them as plain local volumes. The mongo matrix may be
   validating a different topology than the one the compose file declares.
9. Worth adding: a `go test -fuzz` target over crafted tar headers feeding
   `extractTarReader`, and a `-race` run of the concurrent flat-backup test.

---

## Suggested order of work

1. B1 (archive-kind check) — smallest fix, largest blast radius.
2. B3 + B4 (`fileMeta` only for materialised entries; skip sockets) — both are in
   the same two switch statements.
3. B2 (`io.CopyN` + pad/truncate + warn) — makes hot backups usable at all.
4. H1, H2, H3 — CLI-level, all cheap.
5. H4, H5.
6. Test suite items 1-5, which lock all of the above.

---

# Follow-up full review — 2026-08-09

The entire repository was reviewed again after the 2026-08-08 remediation. All
confirmed defects below were fixed and pinned by regression tests.

## Fixed findings

| Severity | Finding | Regression coverage |
|---|---|---|
| Blocker | Bundle entries could use a non-canonical path to escape their logical volume; hard links could cross volume boundaries. | `TestBundleEntryCannotTraverseOutsideItsVolumeRoot`, `TestBundleHardLinkCannotCrossVolumeBoundary` |
| Blocker | An oversized first `manifest.json` could bypass bundle classification and reach a single-volume restore. | `TestOversizedBundleManifestCannotBypassArchiveKindCheck` |
| High | Two bundle names resolving to the same physical directory could stage and swap the same live target twice. | `TestBundleRestoreRejectsDuplicatePhysicalTargets` |
| High | New replacement journals inferred commit state from path existence, so a crash after rename but before durable publication could preserve an uncommitted target. | `TestJournalRollsBackRenameNotDurablyPublished` plus the journal crash matrix |
| High | A crashed flat-index overwrite could lose the previous index entry or attach old metadata to a newly published inode. | `TestFailedFlatOverwriteRestoresPreviousIndexEntry`, `TestFlatOverwriteCrashDistinguishesOldAndPublishedArchive` |
| High | Docker container-usage lookup failed open, allowing restore while safety state was unknown. It now fails closed unless `--force` is explicit. | `TestRestoreRefusesWhenContainerUsageCannotBeChecked` |
| High | The previous Go baseline was affected by GO-2026-4869 in `archive/tar`, reachable from archive verification. Minimum Go is now 1.26.2. | `govulncheck ./...` |
| Medium | `Store.Create` used a check-then-write metadata path and admitted concurrent double creation. Metadata publication is now exclusive, synced and atomic. | `TestConcurrentCreateHasSingleWinner` |
| Medium | Temporary archives inherited the final public mode while still incomplete. They remain `0600` until validation and publication. | `TestTemporaryArchiveStaysPrivateUntilPublication` |
| Medium | Archive discovery accepted FIFOs and verification/restore could block opening one. Only regular files are accepted and opens are non-blocking until type validation. | `TestArchiveDiscoveryAndVerifyRejectFIFOs` |
| Medium | Symlink, FIFO and device xattrs were not archived; concurrent xattr shrink could append zero bytes. | `TestFifoRoundTrip`, `TestSymlinkXattrsRoundTripWhenSupported` |
| Medium | Device major/minor values could overflow during restore. | `TestRestoreRejectsOverflowingDeviceNumbers` |
| Medium | Several exported stream APIs panicked on nil endpoints and repository methods accepted invalid names. | `TestNilStreamEndpointsReturnErrorsInsteadOfPanicking`, `TestInvalidName` |
| Medium | CLI paths silently ignored extra positional arguments, multiple Compose files and duplicate physical volume mappings. | `TestCLIRejectsArgumentsItWouldOtherwiseIgnore`, `TestResolveComposeFileRejectsSilentlyIgnoredOverrides` |
| Medium | Automatic Compose restore trusted filename prefixes and could choose a newer single-volume decoy. It now validates kind and manifest contents. | `TestLatestBundleFindsTimestampedAndRenamedBundles` |
| Low | Archive stems accepted glob/control/path metacharacters; archive locks and reservations were unnecessarily public; several cleanup and reporting errors were ignored. | Expanded validation and regression suites |

## Validation

- `make check`: vet, unit/regression/fidelity/concurrency tests, race detector
  and static build pass.
- `staticcheck ./...`, filtered `gosec`, `govulncheck ./...`, ShellCheck warning
  level and shell syntax checks pass.
- `FuzzExtractTarStream` completed a 30-second run with 334,753 executions and
  no failure.
- `sudo make acceptance` passes end to end: defect scenarios, PostgreSQL/Odoo,
  MongoDB, both client paths and streaming/rclone across all compressions.

## Remaining architectural limits

No further confirmed correctness defect remains open from this review. The
documented product limits still apply: multi-volume publication is atomic per
volume rather than transactionally atomic across filesystems; live backup is
crash-consistent per file rather than a snapshot; sparse extents expand in the
archive; dvol intentionally has no retention/prune command. Compose YAML merge,
environment interpolation and profile evaluation are delegated to
`docker compose config` and are now documented explicitly.
