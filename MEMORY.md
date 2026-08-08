# Session memory — 2026-08-08 :: 12-10

Context for picking this work back up. Architecture, invariants and quality
gates live in `AGENTS.md` (`CLAUDE.md` is a symlink to it); the audit that
started all of this is `REVIEW.md`.

## What happened

1. **Full audit** of the project (`REVIEW.md`): every non-test source file, all
   Go tests, the three acceptance suites and README/Specs. Baseline was green
   (`go vet`, `go test`, and `sudo make acceptance` end to end), so every finding
   below was reproduced with a throwaway probe test before being written down.
2. **All findings fixed** on branch `review-fixes`, each with a unit/regression
   test named after the symptom and, where Docker is what makes it observable, a
   scenario in `test/acceptance_defects_e2e.sh`.
3. New suites added: regression, fidelity, concurrency (race), journal, CLI
   child-process tests.
4. Docs rewritten: `AGENTS.md` (architecture + 17 invariants + gates),
   `README.md` (corrected claims, new flags and commands).

## Defects fixed

Severity as recorded in `REVIEW.md`.

| id | defect | fix | test |
| --- | --- | --- | --- |
| B1 | A bundle restored through the single-volume path silently replaced the volume with the bundle layout | archives are classified by their leading `manifest.json`; `ErrBundleArchive` / `ErrNotBundleArchive` both directions | `TestRestoreRejectsBundleArchiveOnSingleVolume`, acceptance `B1` |
| B2 | A file changing size during the read aborted the whole backup | `copyFileEntry` honours the header size: pad on shrink, truncate on growth, warn | `TestCopyFileEntry*`, `TestBackupSurvivesConcurrentWriter`, acceptance `B2` |
| B3 | An archive containing a FIFO could never be restored (chown on a skipped path) | `extractEntry` reports what it materialised; FIFOs are created, device nodes attempted | `TestFifoRoundTrip`, acceptance `B3/B4` |
| B4 | A unix socket in the volume aborted the backup | sockets are skipped with a warning (`Config.StrictEntries` to make it fatal) | `TestBackupSkipsUnixSocket` |
| H1 | `restore volA volB` treated a *missing* volume as a backup selector | selector recognised by shape, plus explicit `--archive` | `TestLooksLikeArchiveSelector`, `TestRestoreMultipleVolumesWhenOneIsMissing` |
| H2 | `compose-restore` could not find `--timestamp` / `--archive-name` bundles | prefix match on `<project>[-_]`, manifest-based fallback | `TestLatestBundleFindsTimestampedAndRenamedBundles` |
| H3 | compose backup/restore disagreed on the volume set; restore created volumes before validating | `--skip-missing` on backup, `--strict`/`AllowMissing` on restore, targets resolved through `VolumeTargetResolver` only after the manifest check | `TestComposeBackupRefusesMissingVolumeByDefault`, `TestComposeRestoreDoesNotCreateVolumesOnManifestMismatch` |
| H4 | One damaged `.dvol-index.json` entry blocked listing *and* new backups | bad entries dropped with a warning; only undecodable/unknown-version is fatal | `TestFlatIndexSurvivesDamagedEntry` |
| H5 | Nothing stopped a restore into a volume a running container had mounted | `docker ps --filter volume=` guard, `--force` to override | `TestRestoreRefusesVolumeMountedByRunningContainer` |
| H6 | Crash recovery scanned `<root>/volumes` and so never ran for Docker mountpoints | replacement journal under `<root>/state/replacements`, flocked per record | `journal_test.go`, acceptance `H6` |
| M1 | `O_NOATIME` failures were a dead end | `--allow-atime` opt-in, error names the flag | `TestAtimeStrictnessAndOptIn` |
| M2 | Symlink atime restore failed on read-only sources | timestamps re-checked; unchanged times are not an error | in `restoreSymlinkTimes` |
| M3 | Collisions surfaced as a raw `link: file exists` | `ErrArchiveExists` naming `--overwrite`; `--overwrite` added to compose-backup | `TestArchiveCollisionErrorNamesTheFlag` |
| M4 | `RestoreBundleSources` decompressed the archive once per volume | one shared single-pass implementation for every bundle restore | `TestRoundTripFidelityBundle` |
| M5 | Restore held one metadata record per entry; backup materialised a `FileInfo` per directory entry | only directories are deferred; traversal uses `Readdirnames` | — |
| M6 | Bundle prefix was interpolated into a path unvalidated | `checkArchiveStem` | `TestBundlePrefixCannotEscapeDestination` |
| M7 | xattrs, POSIX ACLs and capabilities were silently dropped | PAX `SCHILY.xattr.*` round trip | `TestXattrsRoundTrip` |
| M8 | The index lock was held for the whole archive write | short critical sections + flocked per-archive reservation | `TestListBackupsNotBlockedByInFlightBackup` |
| M9 | No cross-process protection around a swap | journal records are flocked; recovery skips what it cannot lock | `TestJournalDoesNotDisturbInFlightReplacement` |
| M10 | Sparse files were materialised block for block | `sparseFileWriter` seeks over zero runs at 4 KiB granularity | `TestSparseFileStaysSparseOnRestore` |
| L1-L8 | archive mode, dead lz4 branch, `(&Store{})` hack, eager dirs, flag parsing, nil docker client, double `Close`, usage text | see `git log` | — |
| L10 | No way to check an archive without restoring it | `docker dvol verify` | `TestCmdVerifyReportsAndFails` |
| — | **Found while testing the fixes**: `archive/tar` picks USTAR when the format is unspecified, truncating every mtime to whole seconds and dropping the access time | headers are always `tar.FormatPAX` | `TestRoundTripFidelityAllCompressions` |

## Deliberately not done

- **Prune / retention** (`REVIEW.md` L9). Deleting a user's backups is a
  destructive feature: it needs its own design (policy, dry-run, confirmation)
  rather than being smuggled into a bug-fix branch.
- **Sparse extents in the archive**: `archive/tar` cannot write them. Holes are
  reconstructed on restore instead; an archive of a sparse file is still as
  large as its apparent size.
- The MongoDB acceptance matrix declares bind-backed volumes
  (`driver_opts: type=none,o=bind`) but a dvol restore recreates them as plain
  local volumes, so after the first restore the matrix validates a different
  topology than the compose file declares. It passes, but it is worth revisiting
  if that suite is ever extended.

## API changes for embedders

- `Config` gained `AllowAtime`, `ArchiveMode`, `StrictEntries`.
- `ListArchives` is now a package function (the method remains as a wrapper).
- Bundle restores go through `RestoreBundleTargets` / `RestoreBundleReader` with
  `RestoreOptions` and a `VolumeTargetResolver`; the old map-based
  `RestoreBundleSources` / `RestoreBundleSourcesReader` are kept as wrappers.
- New: `InspectArchive`, `Store.VerifyArchive`, `ArchiveReport`, and the
  exported errors `ErrBundleArchive`, `ErrNotBundleArchive`, `ErrArchiveExists`,
  `ErrAtimeUnavailable`.
- `walkReadOnly`, `openReadOnlyDir`, `openReadOnlyAt` take a `readOptions`.
- Removed: `Store.recoverInterruptedReplacements` (replaced by the journal).

## New CLI surface

```
docker dvol verify <volume> [backup] | verify --archive <path> [--json]
docker dvol restore [--archive <path>] [--force] [--allow-missing] ...
docker dvol compose-backup [--skip-missing] [--overwrite] ...
docker dvol compose-restore [--strict] [--force] ...
global: --archive-mode <octal>  --allow-atime
```

## How to resume

```bash
git switch review-fixes
make check                 # vet + test + race + build
sudo make acceptance       # ~35 min, needs Docker; defect suite runs first
```

The sudoers rule at `/etc/sudoers.d/docker-volume-acceptance` allows exactly
`/usr/bin/make -C <repo> acceptance`, and the target refuses to start without
`bin/dvol-cli-plugin`, so run `make build` as the normal user first.

The work sits on branch `review-fixes` as **uncommitted** changes (nothing was
committed or pushed, since that was not asked for); `main` still holds the
audited code. `git status` lists the modified files plus the new
`journal.go`, `restore.go`, `sparse.go`, `xattr.go`, the four new test files,
`test/acceptance_defects_e2e.sh`, `REVIEW.md` and this file.
