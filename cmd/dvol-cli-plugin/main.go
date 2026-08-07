// Command dvol-cli-plugin is the Docker CLI plugin `docker-dvol`. It plugs
// into the `docker` command and exposes volume backup/restore from inside
// Docker, plus compose-aware single-archive backups.
//
// Install as ~/.docker/cli-plugins/docker-dvol (or docker-dvol-linux) to get:
//
//	docker dvol backup|restore|list|backups
//	docker dvol compose-backup|-restore [-f file.yml]
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fabio/dvol/pkg/dvol"
)

const (
	pluginName    = "dvol"
	pluginVersion = "0.1.0"
	metadataCmd   = "docker-cli-plugin-metadata"
)

type app struct {
	store  *dvol.Store
	docker dockerVolumeClient
	out    *tabwriter.Writer
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

	root, backupDir, args := extractGlobalFlags(args)
	if root == "" {
		root = defaultBackupRoot()
	}
	store, err := dvol.NewStore(dvol.Config{Root: root, BackupsDir: backupDir})
	if err != nil {
		fail(err)
	}

	a := &app{store: store, docker: newDockerCLI()}
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

// extractGlobalFlags pulls --root and the backup destination flags out of args.
func extractGlobalFlags(args []string) (root, backupDir string, out []string) {
	out = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--root":
			if i+1 < len(args) {
				root = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		case a == "--backup-dir":
			if i+1 < len(args) {
				backupDir = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--backup-dir="):
			backupDir = strings.TrimPrefix(a, "--backup-dir=")
		case a == "--destination":
			if i+1 < len(args) {
				backupDir = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--destination="):
			backupDir = strings.TrimPrefix(a, "--destination=")
		default:
			out = append(out, a)
		}
	}
	return root, backupDir, out
}

// volumeSource resolves a Docker volume to the mountpoint reported by Docker
// Engine. The backup store contains archives only; volume data always remains
// in Docker's volume.
func (a *app) volumeSource(name string) (string, error) {
	if a.docker == nil {
		return "", errDockerVolumeNotFound
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

func (a *app) restoreVolume(name, archive string) error {
	source, err := a.volumeSource(name)
	if errors.Is(err, errDockerVolumeNotFound) {
		info, err := a.docker.Create(name)
		if err != nil {
			return fmt.Errorf("create Docker volume %q: %w", name, err)
		}
		source = info.Mountpoint
	} else if err != nil {
		return err
	}
	return a.store.RestoreSource(name, source, archive)
}

func (a *app) isVolume(name string) bool {
	if a.docker == nil {
		return false
	}
	_, err := a.docker.Inspect(name)
	return err == nil
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

// cmdBackup: docker dvol backup [options] <volume>...
func (a *app) cmdBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	comp := fs.String("compression", "gzip", "compression format: gzip, xz, zstd")
	level := fs.Int("level", 0, "compression level (0 = format default)")
	var archiveNames multiFlag
	fs.Var(&archiveNames, "archive-name", "archive name stem or volume::archive-name (repeatable)")
	fs.Var(&archiveNames, "name", "alias for --archive-name")
	includeTimestamp := false
	fs.BoolVar(&includeTimestamp, "timestamp", false, "append _ddmmyyyy_HHMMSS to the archive name")
	sameDestination := false
	fs.BoolVar(&sameDestination, "same-destination", false, "store all archives directly in the destination directory")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	opts := dvol.BackupOptions{Compression: dvol.Compression(*comp), Level: *level, Timestamp: includeTimestamp, SameDestination: sameDestination}
	if err := opts.Validate(); err != nil {
		fail(err)
	}
	names := expandNames(fs.Args())
	if len(names) == 0 {
		fail("usage: docker dvol backup [--destination DIR] [--archive-name NAME|VOLUME::NAME] [--same-destination] [--timestamp] [--compression gzip|xz|zstd] [--level N] <volume>...")
	}
	archiveMap, err := parseArchiveNames(archiveNames, names)
	if err != nil {
		fail(err)
	}
	timestampValue := ""
	if includeTimestamp {
		timestampValue = time.Now().Format("02012006_150405")
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

// cmdRestore: docker dvol restore <volume>... [backup]
func (a *app) cmdRestore(args []string) {
	if len(args) == 0 {
		fail("usage: docker dvol restore <volume>... [backup]")
	}
	volArgs := args
	sel := ""
	if len(args) > 1 {
		if last := args[len(args)-1]; !a.isVolume(last) {
			sel = last
			volArgs = args[:len(args)-1]
		}
	}
	names := expandNames(volArgs)
	if len(names) == 0 {
		fail("no volumes to restore")
	}
	if sel != "" {
		if len(names) != 1 {
			fail("usage: docker dvol restore <volume> <backup>")
		}
		path, err := a.store.ResolveBackup(names[0], sel)
		if err != nil {
			fail(err)
		}
		if err := a.restoreVolume(names[0], path); err != nil {
			fail(err)
		}
		fmt.Printf("restored %s from %s\n", names[0], path)
		return
	}
	for _, n := range names {
		if err := a.restoreLatest(n); err != nil {
			fail(err)
		}
	}
}

func (a *app) restoreLatest(name string) error {
	infos, err := a.store.ListBackups(name)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return fmt.Errorf("no backups found for volume %q", name)
	}
	latest := infos[len(infos)-1]
	if err := a.restoreVolume(name, latest.Path); err != nil {
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
	comp := fs.String("compression", "gzip", "compression format: gzip, xz, zstd")
	level := fs.Int("level", 0, "compression level (0 = format default)")
	archiveName := ""
	fs.StringVar(&archiveName, "archive-name", "", "archive name stem (suffix is added automatically)")
	fs.StringVar(&archiveName, "name", "", "alias for --archive-name")
	includeTimestamp := false
	fs.BoolVar(&includeTimestamp, "timestamp", false, "append _ddmmyyyy_HHMMSS to the archive name")
	fs.Var(&files, "f", "compose file (repeatable)")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}

	opts := dvol.BackupOptions{Compression: dvol.Compression(*comp), Level: *level, ArchiveName: archiveName, Timestamp: includeTimestamp}
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
	names, sources, err := a.existingVolumes(cf)
	if err != nil {
		fail(err)
	}
	if len(names) == 0 {
		fail(fmt.Sprintf("no volumes from %s exist in Docker", path))
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
	fs.Var(&files, "f", "compose file (repeatable)")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	rest := fs.Args()

	path, err := resolveComposeFile(files)
	if err != nil {
		fail(err)
	}
	cf, err := dvol.LoadComposeFile(path, *project)
	if err != nil {
		fail(err)
	}
	names := composeVolumeNames(cf)

	archive := ""
	if len(rest) > 0 {
		archive = rest[0]
	} else {
		archive, err = latestBundle(a.store.BundleDir(), cf.Project)
		if err != nil {
			fail(err)
		}
	}
	targets := make(map[string]string, len(names))
	for _, name := range names {
		source, err := a.volumeSource(name)
		if errors.Is(err, errDockerVolumeNotFound) {
			info, createErr := a.docker.Create(name)
			if createErr != nil {
				fail(fmt.Errorf("create Docker volume %q: %w", name, createErr))
			}
			source = info.Mountpoint
			err = nil
		}
		if err != nil {
			fail(err)
		}
		targets[name] = source
	}
	if err := a.store.RestoreBundleSources(archive, targets); err != nil {
		fail(err)
	}
	fmt.Printf("restored %d volume(s) (%s) from %s\n", len(names), strings.Join(names, ", "), archive)
}

func (a *app) usage() {
	fmt.Fprint(os.Stderr, `docker dvol - transparent backup/restore for Docker volumes

Usage:
  docker dvol list                        list volumes
  docker dvol backups <volume>            list backups of a volume
	  docker dvol backup [--destination DIR] [--archive-name NAME|VOLUME::NAME] [--same-destination] [--timestamp] [--compression gzip|xz|zstd] [--level N] <volume>...  backup one or more volumes
  docker dvol restore <volume>... [backup]  restore volumes (optional backup for a single volume)
	  docker dvol compose-backup [-f file.yml] [-p project] [--compression c] [--level N]   backup all volumes of a compose project into one archive
  docker dvol compose-restore [-f file.yml] [-p project] [archive]  restore them

Global flags: --root <dir>          archive repository (default ~/.local/share/docker-volume-backup)
            --destination <dir>  backup destination (default <root>/backups; --backup-dir is an alias)

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

func (a *app) existingVolumes(cf *dvol.ComposeFile) ([]string, map[string]string, error) {
	var out []string
	sources := map[string]string{}
	for _, v := range cf.Volumes {
		source, err := a.volumeSource(v.Name)
		if err == nil {
			out = append(out, v.Name)
			sources[v.Name] = source
			continue
		}
		if !errors.Is(err, errDockerVolumeNotFound) {
			return nil, nil, err
		}
	}
	sort.Strings(out)
	return out, sources, nil
}

func composeVolumeNames(cf *dvol.ComposeFile) []string {
	out := make([]string, 0, len(cf.Volumes))
	for _, v := range cf.Volumes {
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out
}

func latestBundle(dir, project string) (string, error) {
	infos, err := (&dvol.Store{}).ListArchives(dir)
	if err != nil {
		return "", err
	}
	prefix := project + "-"
	var matches []dvol.BackupInfo
	for _, b := range infos {
		if strings.HasPrefix(b.Name, prefix) {
			matches = append(matches, b)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no compose backup found for project %q in %s", project, dir)
	}
	return matches[len(matches)-1].Path, nil
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
	fmt.Fprintln(os.Stderr, "docker dvol:", v)
	os.Exit(1)
}
