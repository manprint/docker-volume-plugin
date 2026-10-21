# Docker Volume Plugin

Volume backup/restore for Docker, shipped as the Docker CLI plugin `docker dvol`.
Volume data stays in Docker; dvol only owns the archives.

`CLAUDE.md` is a symlink to this file.

## Repo layout

```
AGENTS.md            # this file: architecture, invariants, quality gates
CLAUDE.md            # symlink to AGENTS.md
REVIEW.md            # 2026-08-08 audit; every finding has a test named after it
QA_REVIEW_2026-08-11.md  # QA audit of the post-review code
REVIEW_2026-09-11.md # 2026-09-11 audit: concurrency, Docker adapter, toolchain
README.md            # user documentation (Italian)
Specs.md
Makefile
go.mod
cmd/
  dvol-cli-plugin/   # docker CLI plugin ("docker dvol") + compose backup/restore
pkg/
  dvol/              # core library
    dvol.go          # Config, Store, exported errors, VolumeTargetResolver
    store.go         # repository layout, volume metadata, name validation
    journal.go       # replacement journal + crash recovery + replaceStage
    backup.go        # write path: traversal, tar writing, archive publication
    restore.go       # read path: classification, extraction, bundle restore
    backup_index.go  # flat (--same-destination) archive index and reservations
    compression.go   # gzip/xz/zstd/lz4 writers, readers, format detection
    sparse.go        # hole-preserving file writer used by the restore
    xattr.go         # extended attributes <-> PAX records
    log.go           # logger and progress reporting
test/                # acceptance suites (bash, need root + Docker)
```

## Architecture

Three layers, each with one job:

1. **`cmd/dvol-cli-plugin`** — argument handling, Docker Engine queries (through
   the `docker` CLI) and operator-facing policy: which volumes, which archive,
   refuse or warn. It never touches archive bytes.
2. **`pkg/dvol` write path** (`backup.go`) — walks a source directory read-only
   and produces a tar stream. Every entry point funnels into `Store.addTree`;
   publication goes through `writeArchiveWithProgress` (file) or
   `writeArchiveToWithProgress` (stream).
3. **`pkg/dvol` read path** (`restore.go`) — classifies an archive, extracts into
   staging, then swaps. Every entry point funnels into `extractTarStream` and,
   for multi-volume archives, into `restoreBundleStream`.

Two archive shapes:

- **single volume** — root entry `.`, then the tree.
- **bundle** — `manifest.json` (`{"format":"dvol-bundle-v1","volumes":[...]}`)
  as the *first* entry, then one top-level directory per volume.

The manifest is what distinguishes them, and both directions are checked:
`ErrBundleArchive` when a bundle reaches a single-volume restore,
`ErrNotBundleArchive` for the opposite. This is the fix for the worst defect
found in the audit, where a bundle silently overwrote a volume with the bundle
layout.

`VolumeTargetResolver` is the seam between layers 1 and 3: the library asks for a
target directory *after* it has read and validated the manifest, so a resolver
that creates a Docker volume can never leave one behind for an archive that
turned out not to match.

## Invariants

These are load-bearing. Each is pinned by a named test; breaking one is a
release blocker, not a style question.

**Source (backup)**

1. A backup never modifies the source volume — content, mode, owner, mtime,
   atime, symlink targets and xattrs are all unchanged afterwards. Traversal
   uses `O_NOFOLLOW|O_NOATIME|O_DIRECTORY` descriptors, re-verifies inode
   identity after every open, and restores the atime Linux updates when a
   symlink target is read. `TestBackupNeverModifiesSource`, `source_invariant_test.go`.
2. If the source cannot be read without updating atimes, the backup fails and
   names `--allow-atime` rather than silently touching it. `TestAtimeStrictnessAndOptIn`.
3. The archive, its temporary file, the index and the locks never land inside a
   source volume, and never outside the destination either: every control file
   dvol writes (`.dvol-index.json`, `.dvol-index.lock`, `.dvol-reserve-*.lock`,
   `.dvol-temp.lock`, `.dvol-backup-*`) lives in the archive directory itself.
   A destination whose parent is not writable — the ordinary shape of a mounted
   backup share — is therefore usable.
   `TestBackupRejectsUnsafeSourceAndDestination`,
   `TestBackupSucceedsWhenDestinationParentIsNotWritable`,
   `TestTemporaryArchiveLockIsNotListedAsABackup`.
4. A source that changes while it is read never fails the backup: the header
   size wins, short reads are zero-padded, extra bytes are dropped, and the
   operator gets a warning. `TestBackupSurvivesConcurrentWriter`, `TestCopyFileEntry*`.
5. An entry tar cannot represent (unix socket) is skipped with a warning, not
   fatal — unless `Config.StrictEntries`. `TestBackupSkipsUnixSocket`.

**Archive**

6. An archive is published atomically or not at all. It is written to a locked
   temporary file, synced, then linked (or renamed with `--overwrite`) into
   place; the parent directory is synced afterwards. A crash leaves the previous
   archive valid and no partial file under the final name.
   `TestBackupInterruptionKeepsSourceAndExistingArchive`, `TestBackupProcessCrashKeepsSource`.
7. An existing archive is never replaced without `--overwrite`; the collision
   error says so. `TestArchiveCollisionErrorNamesTheFlag`.
8. Headers are always PAX. With an unspecified format the tar writer settles on
   USTAR, which truncates mtime to whole seconds and drops the access time.
   `TestRoundTripFidelityAllCompressions`.
9. Corruption is detectable: every format's trailer is drained and validated on
   read. `dvol verify` does this without writing anything.
   `TestVerifyArchiveReportsAndDetectsCorruption`.

**Target (restore)**

10. The live target is replaced only after the complete stream has been
    extracted and validated. A corrupt, truncated or hostile archive leaves the
    volume byte-identical. Bundle publication has one journalled group commit:
    a failed swap or crash rolls every target back or completes every target,
    never a mixed generation. `TestRestoreFailureNeverModifiesVolume`,
    `TestBundlePublicationFailureRollsBackEveryVolume`,
    `TestReplacementGroupCrashRecoveryIsAllOrNothing`.
11. No archive entry escapes the volume root: absolute paths, `..`, symlinked
    ancestors, hard links pointing outside and a symlink at the volume root are
    all refused. `TestRestoreRejectsZipSlip`, `TestRestoreRejectsSymlinkEscape`,
    `TestRestoreRejectsSymlinkVolumeRoot`.
12. A restore reproduces the source exactly: content, mode incl. setuid/setgid/
    sticky, owner, nanosecond mtime, symlinks, hard-link *grouping*, FIFOs,
    xattrs (hence POSIX ACLs and capabilities) and holes in sparse files.
    Metadata recreation is strict by default; `BestEffortMetadata`/
    `--best-effort-metadata` is the only explicit downgrade. An extended
    attribute is never dropped silently: a set that changes between the sizing
    and the reading call (`ERANGE`) is retried, and one the caller may not read
    is warned about. `fidelity_test.go`,
    `TestStrictMetadataFailureNeverPublishesPartialRestore`,
    `TestXattrReadRetriesWhenTheAttributeSetGrows`,
    `TestXattrReadStillTreatsUnsupportedFilesystemsAsEmpty`.
13. Metadata is only ever applied to entries that were actually created.
    `TestFifoRoundTrip`, `TestRestoreSkipsDeviceNodeWithoutPrivileges`.
14. Every directory swap is journalled, so a crash inside the one non-atomic
    window is repaired by the next `NewStore` — for any target, including a
    Docker mountpoint outside the repository. Journal creation/removal and the
    extracted filesystem are synced before publication. A recovery pass and a
    record being created race for the same name: the creator retries on a fresh
    name rather than continuing with a record that is no longer on disk, and
    recovery ignores a record whose owner unlinked it while it was being opened.
    Another dvol process merely starting up can therefore neither break an
    in-flight restore nor fail itself. `journal_test.go`,
    `TestJournalRecordSurvivesConcurrentRecoveryRemovingIt`,
    `TestJournalRecordSurvivesConcurrentRecoveryLockingIt`,
    `TestReplacementGroupSurvivesConcurrentRecoveryRemovingIt`,
    `TestConcurrentStoreOpensDoNotStealInFlightJournalRecords`.
15. A restore into a volume a running container has mounted is refused unless
    `--force`; the CLI checks both before staging and immediately before commit.
    `TestRestoreRefusesVolumeMountedByRunningContainer`,
    `TestRestoreRechecksRunningContainersImmediatelyBeforeCommit`.

**Metadata**

16. Damaged flat-index entries are dropped with a warning; they never block
    listing or taking a new backup. Only an undecodable file or an unknown
    version is fatal. `TestFlatIndexSurvivesDamagedEntry`.
17. Concurrency is safe by construction: the index lock is short and never spans
    an archive write; an archive name is owned through a flocked reservation
    file; journal records are flocked so recovery skips in-flight operations,
    and the create-then-lock window is closed by retrying rather than by
    continuing with a stolen record. `concurrency_test.go`, run under `-race`.
18. CLI operations accept plain Docker `local` volumes and local bind-backed
    volumes with `type=none`, an absolute `device`, and `o=bind[,rw]`. Bind
    volumes resolve to the host device rather than Docker's transient `_data`
    mount. Remote daemons, Docker Desktop mountpoints, third-party drivers and
    NFS/CIFS-style `local` options are outside the data-path contract and are
    rejected. `TestVolumeSourceSupportsLocalBindDriverOptions`,
    `TestVolumeSourceRejectsUnsupportedOrRemoteDrivers`.
19. `docker dvol lock` protects a volume with a running, labelled, read-only
    guard container. Docker refuses direct/forced removal, prune and Compose
    removal while the reference exists. Only a guard whose labels and mount
    match is ignored by the restore in-use check or removed by `unlock`.
    Lock/unlock are idempotent. `TestVolumeLockLifecycleIsIdempotent`,
    `TestDockerCLIAdapterCommandsAndParsing`, and the real-Docker lock defect
    acceptance scenario pin the contract.
20. The Docker CLI adapter parses standard output only. Docker writes advisory
    lines to standard error on a healthy daemon, and merging the streams put
    them inside the JSON and the volume lists dvol decodes. Diagnostics still
    reach the operator through the error of a failed command, and the
    missing-volume/missing-container classification still reads them.
    `TestDockerCLIIgnoresWarningsOnStandardError`,
    `TestDockerCLIStillReportsStandardErrorDiagnostics`,
    `TestDockerCLIAdapterClassifiesFailures`.

## Quality gates

```bash
make check                 # go vet + go test + go test -race + build
sudo make acceptance       # every acceptance suite, both client paths
```

`make check` must be clean before any commit. `sudo make acceptance` must be
clean before anything that touches the write path, the restore path or the CLI
contract; it needs a working Docker Engine and pulls test images.

The toolchain is pinned by the `go` directive in `go.mod` (currently Go 1.27.3),
which is also what CI feeds to `actions/setup-go` through `go-version-file`.
Bumping it is a one-line change plus a green `make check`; do not pin a Go
version anywhere else.

| target | what it covers |
| --- | --- |
| `make test` | unit, regression, fidelity and concurrency suites |
| `make test-race` | the same under the race detector |
| `make fuzz` | fuzzes the extraction path (`FUZZTIME=5m` for a longer run) |
| `sudo make acceptance-defects` | one scenario per audited defect, real Docker |
| `sudo make acceptance` | defects + PostgreSQL + Odoo + MongoDB + streaming |
| `sudo make acceptance-mongo` | the MongoDB matrix, both client paths |
| `make acceptance-stream` | stdin/stdout against an rclone remote |
| `make e2e` | Docker volume discovery smoke test |

Rules for new work:

- A bug fix lands with a test named after the symptom, in `regression_test.go`
  (library) or `cli_regression_test.go` (command layer), plus a scenario in
  `test/acceptance_defects_e2e.sh` when Docker is what makes it observable.
- Anything touching concurrency lands with a test in `concurrency_test.go` and
  must pass `make test-race`.
- Anything touching what an archive stores lands with an assertion in
  `fidelity_test.go`: the round trip is compared attribute by attribute, so a
  silently dropped attribute fails the build instead of a customer's restore.
- CLI paths that must *fail* are tested through the child-process helper in
  `cli_regression_test.go`; `fail()` calls `os.Exit`, so a direct call would kill
  the test binary.

## Key decisions

- Archive repository is a local host directory (`--root`, `--destination`);
  Docker keeps owning the volume data.
- Default format is tar.gz; xz, zstd and lz4 are supported, balanced level 6.
- The Docker CLI cannot mount plugins under builtin subcommands, so backup and
  restore are exposed as `docker dvol` and compose support as
  `docker dvol compose-backup|compose-restore`.
- Compose backups pack all project volumes into a single bundle
  (`backups/compose/<project>-<ts>.tar.gz`) with a `manifest.json`.
- Docker volumes are discovered through the Docker CLI (so `DOCKER_HOST`,
  contexts and auth keep working) and operated on through their real mountpoints.
- Archives are `0644` by default (`--archive-mode` to change), owned by whoever
  runs the command.
- There is deliberately no prune/retention command: deleting a user's backups is
  a destructive feature that needs its own design and confirmation UX.
- Volume deletion protection uses a Docker guard reference rather than trying
  to override builtin CLI commands, which Docker CLI plugins cannot do.

## Known limits

- `archive/tar` cannot *write* sparse extents, so a hole is stored as a run of
  zeros: an archive of a sparse file is as large as its apparent size. The
  restore reconstructs the holes.
- Reading a source whose files belong to another user requires root, or
  `--allow-atime` plus read permission.
- Backing up a live volume is crash-consistent per file, not point-in-time.
  Quiesce the containers when the application needs a coherent snapshot.
- The supported operational platform is a local Linux Docker Engine with plain
  or supported bind-backed `local` volumes. Docker CLI plugin discovery starts
  at Docker CLI 19.03, but discovery compatibility does not certify a
  daemon/filesystem combination.
- Volume locks are an operational safeguard, not an authorization boundary.
  A Docker administrator can delete the guard container. Compose 5.x reports a
  protected volume as still in use but may return exit code 0.
