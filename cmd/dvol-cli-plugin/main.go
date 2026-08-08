// Command dvol-cli-plugin is the Docker CLI plugin `docker-dvol`. It plugs
// into the `docker` command and exposes volume backup/restore from inside
// Docker, plus compose-aware single-archive backups.
//
// Install as ~/.docker/cli-plugins/docker-dvol to get:
//
//	docker dvol backup|restore|list|backups|verify
//	docker dvol compose-backup|compose-restore [-f file.yml]
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fabio/dvol/pkg/dvol"
)

const (
	pluginName    = "dvol"
	pluginVersion = "0.2.0"
	metadataCmd   = "docker-cli-plugin-metadata"
)

var cliLogger = dvol.NewLogger(os.Stderr, dvol.LevelInfo)

type app struct {
	store          *dvol.Store
	docker         dockerVolumeClient
	out            *tabwriter.Writer
	destinationSet bool
}

// globalFlags are the options accepted before (and mixed with) a subcommand.
type globalFlags struct {
	root        string
	backupDir   string
	logLevel    string
	allowAtime  bool
	archiveMode string
	rest        []string
}

func main() {
	args := os.Args[1:]

	if len(args) == 1 && args[0] == metadataCmd {
		printMetadata()
		return
	}

	if len(args) > 0 {
		if args[0] == pluginName {
			args = args[1:]
		}
		if len(args) > 0 && args[0] == metadataCmd {
			printMetadata()
			return
		}
	}

	global := extractGlobalFlags(args)
	args = global.rest
	logLevel := dvol.LevelInfo
	var err error
	if global.logLevel != "" {
		logLevel, err = dvol.ParseLogLevel(global.logLevel)
		if err != nil {
			fail(err)
		}
	}
	cliLogger = dvol.NewLogger(os.Stderr, logLevel)
	root := global.root
	if root == "" {
		root = defaultBackupRoot()
	}
	archiveMode, err := parseArchiveMode(global.archiveMode)
	if err != nil {
		fail(err)
	}
	store, err := dvol.NewStore(dvol.Config{
		Root:        root,
		BackupsDir:  global.backupDir,
		Logger:      cliLogger,
		AllowAtime:  global.allowAtime,
		ArchiveMode: archiveMode,
	})
	if err != nil {
		fail(err)
	}

	a := &app{store: store, docker: newDockerCLI(), destinationSet: global.backupDir != ""}
	if len(args) == 0 {
		a.usage()
		return
	}
	switch args[0] {
	case "help", "-h", "--help":
		a.usage()
	case "list", "ls":
		a.cmdList(args[1:])
	case "backups":
		a.cmdBackups(args[1:])
	case "backup":
		a.cmdBackup(args[1:])
	case "restore":
		a.cmdRestore(args[1:])
	case "verify":
		a.cmdVerify(args[1:])
	case "compose-backup":
		a.cmdComposeBackup(args[1:])
	case "compose-restore":
		a.cmdComposeRestore(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "docker %s: unknown command %q\n", pluginName, args[0])
		a.usage()
		os.Exit(2)
	}
}

func defaultBackupRoot() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "docker-volume-backup")
	}
	return dvol.DefaultRoot
}

func printMetadata() {
	fmt.Println(`{"SchemaVersion":"0.1.0","Vendor":"fabio","Version":"` + pluginVersion + `","ShortDescription":"Backup and restore Docker volumes (also from Docker Compose)"}`)
}

func parseArchiveMode(value string) (os.FileMode, error) {
	if value == "" {
		return 0, nil
	}
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil || mode == 0 || mode > 0o777 {
		return 0, fmt.Errorf("invalid --archive-mode %q (use an octal permission such as 0600)", value)
	}
	return os.FileMode(mode), nil
}

// extractGlobalFlags pulls the repository-wide options out of args. Parsing
// stops at "--" so a value that looks like a global flag is never stolen from
// a subcommand.
func extractGlobalFlags(args []string) globalFlags {
	g := globalFlags{rest: make([]string, 0, len(args))}
	takeValue := func(i *int, name string) string {
		if *i+1 < len(args) {
			*i++
			return args[*i]
		}
		fail(fmt.Sprintf("%s requires a value", name))
		return ""
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			g.rest = append(g.rest, args[i+1:]...)
			break
		}
		switch {
		case a == "--root":
			g.root = takeValue(&i, a)
		case strings.HasPrefix(a, "--root="):
			g.root = strings.TrimPrefix(a, "--root=")
		case a == "--backup-dir" || a == "--destination":
			g.backupDir = takeValue(&i, a)
		case strings.HasPrefix(a, "--backup-dir="):
			g.backupDir = strings.TrimPrefix(a, "--backup-dir=")
		case strings.HasPrefix(a, "--destination="):
			g.backupDir = strings.TrimPrefix(a, "--destination=")
		case a == "--log-level":
			g.logLevel = takeValue(&i, a)
		case strings.HasPrefix(a, "--log-level="):
			g.logLevel = strings.TrimPrefix(a, "--log-level=")
		case a == "--archive-mode":
			g.archiveMode = takeValue(&i, a)
		case strings.HasPrefix(a, "--archive-mode="):
			g.archiveMode = strings.TrimPrefix(a, "--archive-mode=")
		case a == "--allow-atime":
			g.allowAtime = true
		default:
			g.rest = append(g.rest, a)
		}
	}
	return g
}

// volumeSource resolves a Docker volume to the mountpoint reported by Docker
// Engine. The backup store contains archives only; volume data always remains
// in Docker's volume.
func (a *app) volumeSource(name string) (string, error) {
	if a.docker == nil {
		return "", fmt.Errorf("Docker CLI is required to resolve volume %q", name)
	}
	info, err := a.docker.Inspect(name)
	if err != nil {
		return "", err
	}
	if info.Mountpoint == "" {
		return "", fmt.Errorf("Docker volume %q has no usable mountpoint", name)
	}
	return info.Mountpoint, nil
}

// volumeTarget resolves the mountpoint of a volume, creating it if needed.
func (a *app) volumeTarget(name string) (string, error) {
	if a.docker == nil {
		return "", fmt.Errorf("Docker CLI is required to resolve volume %q", name)
	}
	source, err := a.volumeSource(name)
	if errors.Is(err, errDockerVolumeNotFound) {
		info, createErr := a.docker.Create(name)
		if createErr != nil {
			return "", fmt.Errorf("create Docker volume %q: %w", name, createErr)
		}
		if info.Mountpoint == "" {
			return "", fmt.Errorf("Docker volume %q has no usable mountpoint", name)
		}
		return info.Mountpoint, nil
	} else if err != nil {
		return "", err
	}
	return source, nil
}

// checkVolumeIdle refuses to replace a volume a running container has mounted.
func (a *app) checkVolumeIdle(name string, force bool) error {
	if a.docker == nil {
		return nil
	}
	containers, err := a.docker.ContainersUsing(name)
	if err != nil {
		// Not being able to ask Docker must not block a restore; warn instead.
		cliLogger.Warn("cannot determine whether the volume is in use",
			dvol.LogField{Key: "volume", Value: name}, dvol.LogField{Key: "error", Value: err})
		return nil
	}
	if len(containers) == 0 {
		return nil
	}
	if force {
		cliLogger.Warn("restoring a volume mounted by running containers",
			dvol.LogField{Key: "volume", Value: name},
			dvol.LogField{Key: "containers", Value: strings.Join(containers, ",")})
		return nil
	}
	return fmt.Errorf("volume %q is mounted by running container(s) %s: a restore replaces the directory they hold open, "+
		"so they would keep writing to the old data. Stop them first, or pass --force",
		name, strings.Join(containers, ", "))
}

func (a *app) restoreVolume(name, archive string, force bool) error {
	if err := a.checkVolumeIdle(name, force); err != nil {
		return err
	}
	target, err := a.volumeTarget(name)
	if err != nil {
		return err
	}
	return a.store.RestoreSource(name, target, archive)
}

// volumeResolver returns a dvol resolver that creates missing Docker volumes.
// It is called by the library only after a bundle manifest has been validated,
// so a mismatching archive cannot leave stray empty volumes behind.
func (a *app) volumeResolver(force bool) dvol.VolumeTargetResolver {
	return func(name string) (string, error) {
		if err := a.checkVolumeIdle(name, force); err != nil {
			return "", err
		}
		return a.volumeTarget(name)
	}
}

// cmdList: docker dvol list
func (a *app) cmdList(args []string) {
	if a.docker == nil {
		fail("Docker CLI is required")
	}
	dockerVolumes, err := a.docker.List()
	if err != nil {
		fail(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDRIVER\tCREATED\tSIZE\tMOUNTPOINT")
	for _, v := range dockerVolumes {
		fmt.Fprintf(tw, "%s\t%s\t-\t-\t%s\n", v.Name, v.Driver, v.Mountpoint)
	}
	tw.Flush()
}

// cmdBackups: docker dvol backups <volume>
func (a *app) cmdBackups(args []string) {
	if len(args) < 1 {
		fail("usage: docker dvol backups <volume>")
	}
	infos, err := a.store.ListBackups(args[0])
	if err != nil {
		fail(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BACKUP\tSIZE\tCREATED")
	for _, b := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, humanSize(b.Size), b.CreatedAt.Format("2006-01-02 15:04"))
	}
	tw.Flush()
}

// cmdVerify: docker dvol verify <volume> [backup] | docker dvol verify --archive <path>
func (a *app) cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	archive := fs.String("archive", "", "verify this archive path directly")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	rest := fs.Args()
	paths := []string{}
	switch {
	case *archive != "":
		paths = append(paths, *archive)
	case len(rest) >= 2:
		path, err := a.store.ResolveBackup(rest[0], rest[1])
		if err != nil {
			fail(err)
		}
		paths = append(paths, path)
	case len(rest) == 1:
		infos, err := a.store.ListBackups(rest[0])
		if err != nil {
			fail(err)
		}
		if len(infos) == 0 {
			fail(fmt.Sprintf("no backups found for volume %q", rest[0]))
		}
		for _, info := range infos {
			paths = append(paths, info.Path)
		}
	default:
		fail("usage: docker dvol verify <volume> [backup] | docker dvol verify --archive <path>")
	}
	failed := 0
	for _, path := range paths {
		report, err := a.store.VerifyArchive(path)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", path, err)
			continue
		}
		if *asJSON {
			b, _ := json.Marshal(report)
			fmt.Println(string(b))
			continue
		}
		kind := "volume"
		if report.Bundle {
			kind = "bundle(" + strings.Join(report.Volumes, ",") + ")"
		}
		fmt.Printf("OK %s: %s, %d entries, %s, %d symlinks, %d hardlinks\n",
			path, kind, report.Entries, humanSize(report.Bytes), report.Symlinks, report.Hardlink)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// cmdBackup: docker dvol backup [options] <volume>...
func (a *app) cmdBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	comp := fs.String("compression", "gzip", "compression format: gzip, xz, zstd or lz4")
	level := fs.Int("level", 0, "compression level (0 = balanced default)")
	threads := fs.Int("threads", 0, "compression threads (0 = half of CPU count; xz uses one)")
	stdout := fs.Bool("stdout", false, "write the archive stream to stdout")
	bundle := fs.Bool("bundle", false, "write multiple volumes as one bundle")
	overwrite := fs.Bool("overwrite", false, "replace an existing local archive")
	var archiveNames multiFlag
	fs.Var(&archiveNames, "archive-name", "archive name stem or volume::archive-name (repeatable)")
	fs.Var(&archiveNames, "name", "alias for --archive-name")
	includeTimestamp := false
	fs.BoolVar(&includeTimestamp, "timestamp", false, "append _ddmmyyyy_HHMMSS to the archive name")
	timestampUTC := false
	fs.BoolVar(&timestampUTC, "timestamp-utc", false, "use UTC instead of local time for --timestamp")
	sameDestination := false
	fs.BoolVar(&sameDestination, "same-destination", false, "store all archives directly in the destination directory")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	opts := dvol.BackupOptions{Compression: dvol.Compression(*comp), Level: *level, Threads: *threads, Timestamp: includeTimestamp, TimestampUTC: timestampUTC, SameDestination: sameDestination, Overwrite: *overwrite}
	if err := opts.Validate(); err != nil {
		fail(err)
	}
	names := expandNames(fs.Args())
	if len(names) == 0 {
		fail("usage: docker dvol backup [options] <volume>...")
	}
	if err := checkDuplicates(names); err != nil {
		fail(err)
	}
	if *stdout {
		if a.destinationSet {
			fail("--stdout cannot be combined with --destination or --backup-dir")
		}
		if len(archiveNames) > 0 || includeTimestamp || timestampUTC || sameDestination || *overwrite {
			fail("--stdout cannot be combined with archive naming, timestamp, --same-destination or --overwrite")
		}
		if len(names) > 1 && !*bundle {
			fail("multiple volumes require --bundle when using --stdout")
		}
		sources := a.mustResolveSources(names)
		var err error
		if *bundle {
			err = a.store.BackupBundleSourcesToWriter(names, sources, opts, os.Stdout)
		} else {
			err = a.store.BackupSourceToWriter(names[0], sources[names[0]], opts, os.Stdout)
		}
		if err != nil {
			fail(err)
		}
		return
	}
	archiveMap, err := parseArchiveNames(archiveNames, names)
	if err != nil {
		fail(err)
	}
	timestampValue := ""
	if includeTimestamp {
		now := time.Now()
		if timestampUTC {
			now = now.UTC()
		}
		timestampValue = now.Format(dvol.TimestampLayout)
	}
	if *bundle {
		if len(archiveNames) > 1 || (len(archiveNames) == 1 && strings.Contains(archiveNames[0], "::")) {
			fail("--archive-name with --bundle accepts one bundle name, not volume::name mappings")
		}
		prefix := "volumes"
		if len(archiveNames) == 1 {
			opts.ArchiveName = archiveNames[0]
		}
		opts.TimestampValue = timestampValue
		sources := a.mustResolveSources(names)
		path, err := a.store.BackupBundleSourcesWithOptions(names, sources, a.store.BundleDir(), prefix, opts)
		if err != nil {
			fail(err)
		}
		fmt.Printf("backed up %d volume(s) (%s) -> %s\n", len(names), strings.Join(names, ", "), path)
		return
	}
	for _, n := range names {
		source, err := a.volumeSource(n)
		if err != nil {
			fail(err)
		}
		currentOpts := opts
		currentOpts.ArchiveName = archiveMap[n]
		currentOpts.TimestampValue = timestampValue
		path, err := a.store.BackupSourceWithOptions(n, source, currentOpts)
		if err != nil {
			fail(err)
		}
		fmt.Printf("backed up %s -> %s\n", n, path)
	}
}

func (a *app) mustResolveSources(names []string) map[string]string {
	sources := make(map[string]string, len(names))
	for _, n := range names {
		source, err := a.volumeSource(n)
		if err != nil {
			fail(err)
		}
		sources[n] = source
	}
	return sources
}

// archiveSelectorSuffixes decide whether a trailing argument names an archive.
var archiveSelectorSuffixes = []string{".tar.gz", ".tar.xz", ".tar.zst", ".tar.zstd", ".tar.lz4"}

// looksLikeArchiveSelector reports whether an argument is meant as a backup
// selector rather than a volume name.
//
// The previous rule asked Docker whether a volume with that name existed, which
// broke the one case that matters: after losing a volume, `docker dvol restore
// web db` reinterpreted the missing "db" as a backup name for "web" and failed
// instead of restoring both. A selector is now recognised by its shape, which
// does not depend on the state we are trying to repair.
func looksLikeArchiveSelector(arg string) bool {
	if strings.ContainsAny(arg, "*?[") || strings.Contains(arg, "/") {
		return true
	}
	for _, suffix := range archiveSelectorSuffixes {
		if strings.HasSuffix(arg, suffix) {
			return true
		}
	}
	return false
}

// cmdRestore: docker dvol restore [--stdin] [--spool] [--bundle] [--force] <volume>... [backup]
func (a *app) cmdRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	stdin := fs.Bool("stdin", false, "read the archive stream from stdin")
	spool := fs.Bool("spool", false, "with --stdin, spool the archive to a temporary file before restoring")
	bundle := fs.Bool("bundle", false, "restore a complete multi-volume bundle")
	force := fs.Bool("force", false, "restore even if running containers mount the volume")
	archiveFlag := fs.String("archive", "", "archive path or glob to restore (implies a single volume)")
	allowMissing := fs.Bool("allow-missing", false, "with --bundle, only warn when a requested volume is absent from the bundle")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	args = fs.Args()
	if *stdin {
		if *archiveFlag != "" {
			fail("--archive cannot be combined with --stdin")
		}
		if len(args) == 0 {
			fail("--stdin requires the target volume names")
		}
		names := expandNames(args)
		if err := checkDuplicates(names); err != nil {
			fail(err)
		}
		if !*bundle && len(names) != 1 {
			fail("multiple stdin volumes require --bundle")
		}
		input, cleanup := stdinInput(*spool, ".dvol-stdin-*.archive")
		defer cleanup()
		var err error
		if *bundle {
			err = a.store.RestoreBundleReader(input,
				dvol.RestoreOptions{Volumes: names, AllowMissing: *allowMissing},
				a.volumeResolver(*force))
		} else {
			name := names[0]
			if err = a.checkVolumeIdle(name, *force); err == nil {
				var target string
				if target, err = a.volumeTarget(name); err == nil {
					err = a.store.RestoreSourceReader(name, target, input)
				}
			}
		}
		if err != nil {
			fail(err)
		}
		fmt.Fprintf(os.Stderr, "restored %d volume(s) from stdin\n", len(names))
		return
	}
	if *spool {
		fail("--spool requires --stdin")
	}
	if *bundle {
		if *archiveFlag == "" && len(args) < 2 {
			fail("usage: docker dvol restore --bundle <volume>... <archive>")
		}
		archive := *archiveFlag
		volArgs := args
		if archive == "" {
			archive = args[len(args)-1]
			volArgs = args[:len(args)-1]
		}
		names := expandNames(volArgs)
		if err := checkDuplicates(names); err != nil {
			fail(err)
		}
		if err := a.store.RestoreBundleTargets(archive,
			dvol.RestoreOptions{Volumes: names, AllowMissing: *allowMissing},
			a.volumeResolver(*force)); err != nil {
			fail(err)
		}
		fmt.Fprintf(os.Stderr, "restored %d volume(s) from %s\n", len(names), archive)
		return
	}

	volArgs := args
	sel := *archiveFlag
	if sel == "" && len(args) > 1 && looksLikeArchiveSelector(args[len(args)-1]) {
		sel = args[len(args)-1]
		volArgs = args[:len(args)-1]
	}
	names := expandNames(volArgs)
	if len(names) == 0 {
		fail("usage: docker dvol restore [--stdin] [--spool] [--bundle] <volume>... [backup]")
	}
	if err := checkDuplicates(names); err != nil {
		fail(err)
	}
	if sel != "" {
		if len(names) != 1 {
			fail("a backup selector restores exactly one volume: docker dvol restore <volume> <backup>")
		}
		path, err := a.store.ResolveBackup(names[0], sel)
		if err != nil {
			fail(err)
		}
		if err := a.restoreVolume(names[0], path, *force); err != nil {
			fail(err)
		}
		fmt.Printf("restored %s from %s\n", names[0], path)
		return
	}
	for _, n := range names {
		if err := a.restoreLatest(n, *force); err != nil {
			fail(err)
		}
	}
}

func stdinInput(spool bool, pattern string) (io.Reader, func()) {
	if !spool {
		return os.Stdin, func() {}
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		fail(err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(f.Name()) }
	if _, err := io.Copy(f, os.Stdin); err != nil {
		cleanup()
		fail(fmt.Errorf("spooling stdin: %w", err))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		fail(err)
	}
	return f, cleanup
}

func (a *app) restoreLatest(name string, force bool) error {
	infos, err := a.store.ListBackups(name)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return fmt.Errorf("no backups found for volume %q", name)
	}
	latest := infos[len(infos)-1]
	if err := a.restoreVolume(name, latest.Path, force); err != nil {
		return err
	}
	fmt.Printf("restored %s from %s\n", name, latest.Path)
	return nil
}

// cmdComposeBackup: docker dvol compose-backup [-f file] [-p project] [options]
func (a *app) cmdComposeBackup(args []string) {
	fs := flag.NewFlagSet("compose-backup", flag.ContinueOnError)
	var files multiFlag
	project := fs.String("p", "", "project name")
	comp := fs.String("compression", "gzip", "compression format: gzip, xz, zstd or lz4")
	level := fs.Int("level", 0, "compression level (0 = balanced default)")
	threads := fs.Int("threads", 0, "compression threads (0 = half of CPU count; xz uses one)")
	stdout := fs.Bool("stdout", false, "write the bundle stream to stdout")
	overwrite := fs.Bool("overwrite", false, "replace an existing bundle with the same name")
	skipMissing := fs.Bool("skip-missing", false, "back up only the declared volumes that exist in Docker")
	archiveName := ""
	fs.StringVar(&archiveName, "archive-name", "", "archive name stem (suffix is added automatically)")
	fs.StringVar(&archiveName, "name", "", "alias for --archive-name")
	includeTimestamp := false
	fs.BoolVar(&includeTimestamp, "timestamp", false, "append _ddmmyyyy_HHMMSS to the archive name")
	timestampUTC := false
	fs.BoolVar(&timestampUTC, "timestamp-utc", false, "use UTC instead of local time for --timestamp")
	fs.Var(&files, "f", "compose file (repeatable)")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}

	opts := dvol.BackupOptions{Compression: dvol.Compression(*comp), Level: *level, Threads: *threads, ArchiveName: archiveName, Timestamp: includeTimestamp, TimestampUTC: timestampUTC, Overwrite: *overwrite}
	if err := opts.Validate(); err != nil {
		fail(err)
	}

	path, err := resolveComposeFile(files)
	if err != nil {
		fail(err)
	}
	cf, err := dvol.LoadComposeFile(path, *project)
	if err != nil {
		fail(err)
	}
	names, sources, missing, err := a.existingVolumes(cf)
	if err != nil {
		fail(err)
	}
	if len(missing) > 0 {
		// Silently backing up a subset produces an archive that no longer
		// matches the project, and the gap only surfaces during a restore.
		if !*skipMissing {
			fail(fmt.Sprintf("volume(s) %s declared in %s do not exist in Docker; "+
				"create them or pass --skip-missing to back up only the existing ones",
				strings.Join(missing, ", "), path))
		}
		cliLogger.Warn("compose volumes missing from Docker are not in the bundle",
			dvol.LogField{Key: "volumes", Value: strings.Join(missing, ",")})
	}
	if len(names) == 0 {
		fail(fmt.Sprintf("no volumes from %s exist in Docker", path))
	}
	if *stdout {
		if a.destinationSet {
			fail("--stdout cannot be combined with --destination or --backup-dir")
		}
		if archiveName != "" || includeTimestamp || timestampUTC || *overwrite {
			fail("--stdout cannot be combined with archive naming, timestamp or --overwrite")
		}
		if err := a.store.BackupBundleSourcesToWriter(names, sources, opts, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	out, err := a.store.BackupBundleSourcesWithOptions(names, sources, a.store.BundleDir(), cf.Project, opts)
	if err != nil {
		fail(err)
	}
	fmt.Printf("backed up %d volume(s) (%s) -> %s\n", len(names), strings.Join(names, ", "), out)
}

// cmdComposeRestore: docker dvol compose-restore [-f file] [-p project] [archive]
func (a *app) cmdComposeRestore(args []string) {
	fs := flag.NewFlagSet("compose-restore", flag.ContinueOnError)
	var files multiFlag
	project := fs.String("p", "", "project name")
	stdin := fs.Bool("stdin", false, "read the bundle stream from stdin")
	spool := fs.Bool("spool", false, "with --stdin, spool the bundle before restoring")
	force := fs.Bool("force", false, "restore even if running containers mount the volumes")
	strict := fs.Bool("strict", false, "fail when a declared volume is absent from the bundle")
	fs.Var(&files, "f", "compose file (repeatable)")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	rest := fs.Args()
	if *spool && !*stdin {
		fail("--spool requires --stdin")
	}
	if *stdin && len(rest) > 0 {
		fail("--stdin cannot be combined with an archive path")
	}

	path, err := resolveComposeFile(files)
	if err != nil {
		fail(err)
	}
	cf, err := dvol.LoadComposeFile(path, *project)
	if err != nil {
		fail(err)
	}
	names := composeVolumeNames(cf)
	if len(names) == 0 {
		fail(fmt.Sprintf("%s declares no named volumes", path))
	}
	opts := dvol.RestoreOptions{Volumes: names, AllowMissing: !*strict}

	if *stdin {
		input, cleanup := stdinInput(*spool, ".dvol-compose-stdin-*.archive")
		defer cleanup()
		if err := a.store.RestoreBundleReader(input, opts, a.volumeResolver(*force)); err != nil {
			fail(err)
		}
		fmt.Fprintf(os.Stderr, "restored compose project %s from stdin\n", cf.Project)
		return
	}
	archive := ""
	if len(rest) > 0 {
		archive = rest[0]
	} else {
		archive, err = latestBundle(a.store.BundleDir(), cf.Project, names)
		if err != nil {
			fail(err)
		}
	}
	if err := a.store.RestoreBundleTargets(archive, opts, a.volumeResolver(*force)); err != nil {
		fail(err)
	}
	fmt.Printf("restored compose project %s (%s) from %s\n", cf.Project, strings.Join(names, ", "), archive)
}

func (a *app) usage() {
	fmt.Fprint(os.Stderr, `docker dvol - transparent backup/restore for Docker volumes

Usage:
  docker dvol list                          list volumes
  docker dvol backups <volume>              list backups of a volume
  docker dvol backup [options] <volume>...  back up one or more volumes
  docker dvol restore [options] <volume>... [backup]
  docker dvol verify <volume> [backup] | verify --archive <path>
  docker dvol compose-backup [-f file.yml] [-p project] [options]
  docker dvol compose-restore [-f file.yml] [-p project] [--stdin] [--spool] [archive]

Global flags:
  --root <dir>          archive repository (default ~/.local/share/docker-volume-backup)
  --destination <dir>   backup destination (default <root>/backups; --backup-dir is an alias)
  --log-level <level>   TRACE, DEBUG, INFO (default), WARN or ERROR
  --archive-mode <oct>  permission of published archives (default 0644)
  --allow-atime         allow reading sources that cannot be opened with O_NOATIME

backup flags:
  --compression <gzip|xz|zstd|lz4>  --level <n>  --threads <n>
  --archive-name VALUE | VOLUME::NAME (repeatable)   --timestamp   --timestamp-utc
  --same-destination   --overwrite   --bundle   --stdout

restore flags:
  --archive <path|glob>  restore this archive (single volume)
  --bundle               restore a multi-volume bundle
  --stdin [--spool]      read the archive from stdin
  --allow-missing        with --bundle, warn instead of failing on absent volumes
  --force                restore even if running containers mount the volume

`)
}

// expandNames accepts space- and comma-separated volume names.
func expandNames(args []string) []string {
	var out []string
	for _, a := range args {
		for _, p := range strings.Split(a, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func checkDuplicates(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return fmt.Errorf("volume %q listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// parseArchiveNames accepts either one legacy name for one volume or
// repeatable volume::archive-name mappings. If mappings are used, every
// selected volume must be mapped exactly once.
func parseArchiveNames(values []string, volumes []string) (map[string]string, error) {
	result := make(map[string]string)
	if len(values) == 0 {
		return result, nil
	}
	selected := make(map[string]bool, len(volumes))
	for _, volume := range volumes {
		selected[volume] = true
	}
	for _, value := range values {
		volume, archive, mapped := strings.Cut(value, "::")
		if !mapped {
			if len(values) != 1 || len(selected) != 1 {
				return nil, fmt.Errorf("--archive-name must use VOLUME::NAME for multiple volumes")
			}
			for volume = range selected {
				archive = value
			}
		} else if !selected[volume] {
			return nil, fmt.Errorf("--archive-name refers to volume %q, which is not being backed up", volume)
		}
		if volume == "" || archive == "" {
			return nil, fmt.Errorf("invalid --archive-name %q (use VOLUME::NAME)", value)
		}
		if _, exists := result[volume]; exists {
			return nil, fmt.Errorf("duplicate --archive-name mapping for volume %q", volume)
		}
		if err := (dvol.BackupOptions{ArchiveName: archive}).Validate(); err != nil {
			return nil, err
		}
		result[volume] = archive
	}
	if len(result) != len(selected) {
		return nil, fmt.Errorf("when using --archive-name mappings, every selected volume must have a mapping")
	}
	if len(selected) > 1 {
		seen := make(map[string]string, len(result))
		for volume, archive := range result {
			if previous, exists := seen[archive]; exists {
				return nil, fmt.Errorf("archive name %q is used for both %q and %q", archive, previous, volume)
			}
			seen[archive] = volume
		}
	}
	return result, nil
}

// multiFlag is a repeatable string flag (flag pkg has no StringSlice).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func resolveComposeFile(files []string) (string, error) {
	if len(files) > 0 {
		return files[0], nil
	}
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		if _, err := os.Stat(name); err == nil {
			abs, _ := filepath.Abs(name)
			return abs, nil
		}
	}
	return "", fmt.Errorf("no compose file found (looked for compose.yaml, compose.yml, docker-compose.yml); use -f")
}

// existingVolumes splits the declared compose volumes into the ones Docker
// knows about and the ones it does not.
func (a *app) existingVolumes(cf *dvol.ComposeFile) (names []string, sources map[string]string, missing []string, err error) {
	sources = map[string]string{}
	for _, v := range cf.Volumes {
		source, err := a.volumeSource(v.Name)
		if err == nil {
			names = append(names, v.Name)
			sources[v.Name] = source
			continue
		}
		if !errors.Is(err, errDockerVolumeNotFound) {
			return nil, nil, nil, err
		}
		missing = append(missing, v.Name)
	}
	sort.Strings(names)
	sort.Strings(missing)
	return names, sources, missing, nil
}

func composeVolumeNames(cf *dvol.ComposeFile) []string {
	out := make([]string, 0, len(cf.Volumes))
	for _, v := range cf.Volumes {
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out
}

// bundleBelongsToProject matches the archive names compose-backup produces:
// "<project>-<high precision ts>" without --timestamp and "<project>_<stamp>"
// with it. Matching only "-" silently hid every timestamped bundle.
func bundleBelongsToProject(name, project string) bool {
	rest, ok := strings.CutPrefix(name, project)
	if !ok {
		return false
	}
	return strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, "_")
}

// latestBundle finds the newest bundle for a project. Names produced with
// --archive-name carry no project prefix, so the search falls back to reading
// the manifest of the candidate archives, newest first.
func latestBundle(dir, project string, volumes []string) (string, error) {
	infos, err := dvol.ListArchives(dir)
	if err != nil {
		return "", err
	}
	for i := len(infos) - 1; i >= 0; i-- {
		if bundleBelongsToProject(infos[i].Name, project) {
			return infos[i].Path, nil
		}
	}
	want := map[string]bool{}
	for _, v := range volumes {
		want[v] = true
	}
	for i := len(infos) - 1; i >= 0; i-- {
		report, err := dvol.InspectArchive(infos[i].Path)
		if err != nil || !report.Bundle || len(report.Volumes) != len(want) {
			continue
		}
		match := true
		for _, v := range report.Volumes {
			if !want[v] {
				match = false
				break
			}
		}
		if match {
			return infos[i].Path, nil
		}
	}
	return "", fmt.Errorf("no compose backup found for project %q in %s (pass the archive path explicitly)", project, dir)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func fail(v any) {
	cliLogger.Error("command failed", dvol.LogField{Key: "error", Value: v})
	fmt.Fprintln(os.Stderr, "docker dvol:", v)
	os.Exit(1)
}
