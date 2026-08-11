package main

// CLI regression suite for the review findings that live in the command layer.
//
// The command functions terminate the process through fail(), so the paths that
// must *fail* are exercised in a child process: the parent describes the whole
// fixture (repository root, fake Docker volumes, arguments) in one environment
// variable and asserts on the exit code and the message the operator sees.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fabio/dvol/pkg/dvol"
)

const childEnv = "DVOL_CLI_CHILD_SPEC"

type childSpec struct {
	Root      string              `json:"root"`
	Volumes   map[string]string   `json:"volumes"`
	Creatable map[string]string   `json:"creatable"`
	InUse     map[string][]string `json:"in_use"`
	InUseErr  string              `json:"in_use_error"`
	Command   string              `json:"command"`
	Args      []string            `json:"args"`
}

// TestCLIChild is the child-process entry point. It is inert in a normal run.
func TestCLIChild(t *testing.T) {
	raw := os.Getenv(childEnv)
	if raw == "" {
		t.Skip("child process helper")
	}
	var spec childSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "bad child spec:", err)
		os.Exit(3)
	}
	store, err := dvol.NewStore(dvol.Config{Root: spec.Root, Logger: cliLogger})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child store:", err)
		os.Exit(3)
	}
	fake := &fakeDockerVolumes{
		volumes:           map[string]dockerVolumeInfo{},
		createMountpoints: spec.Creatable,
		inUse:             spec.InUse,
	}
	if spec.InUseErr != "" {
		fake.inUseErr = fmt.Errorf("%s", spec.InUseErr)
	}
	for name, mountpoint := range spec.Volumes {
		fake.volumes[name] = dockerVolumeInfo{Name: name, Driver: "local", Mountpoint: mountpoint}
	}
	a := &app{store: store, docker: fake}
	switch spec.Command {
	case "restore":
		a.cmdRestore(spec.Args)
	case "backup":
		a.cmdBackup(spec.Args)
	case "lock":
		a.cmdLock(spec.Args)
	case "unlock":
		a.cmdUnlock(spec.Args)
	case "compose-backup":
		a.cmdComposeBackup(spec.Args)
	case "compose-restore":
		a.cmdComposeRestore(spec.Args)
	case "verify":
		a.cmdVerify(spec.Args)
	default:
		fmt.Fprintln(os.Stderr, "unknown child command:", spec.Command)
		os.Exit(3)
	}
	os.Exit(0)
}

// runChild executes one CLI command in a child process.
func runChild(t *testing.T, spec childSpec) (stdout, stderr string, code int) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIChild$")
	cmd.Env = append(os.Environ(), childEnv+"="+string(raw))
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	code = 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running child: %v (stderr: %s)", err, errBuf.String())
	}
	return out.String(), errBuf.String(), code
}

func TestVolumeLockCLIRejectsMissingAndMultipleVolumes(t *testing.T) {
	root := t.TempDir()
	_, stderr, code := runChild(t, childSpec{
		Root: root, Command: "lock", Args: []string{"missing"},
	})
	if code == 0 || !strings.Contains(stderr, "docker volume not found") {
		t.Fatalf("missing volume lock was not rejected: code=%d stderr=%s", code, stderr)
	}

	_, stderr, code = runChild(t, childSpec{
		Root: root, Command: "lock", Args: []string{"one", "two"},
	})
	if code == 0 || !strings.Contains(stderr, "usage: docker dvol lock") {
		t.Fatalf("multiple-volume lock was not rejected: code=%d stderr=%s", code, stderr)
	}
}

func seedCLIVolume(t *testing.T, store *dvol.Store, name, content string) string {
	t.Helper()
	if err := store.Create(name, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.VolumePath(name), "data.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Backup(name); err != nil {
		t.Fatal(err)
	}
	return store.VolumePath(name)
}

// ---------------------------------------------------------------------------
// H1: `docker dvol restore web db` reinterpreted a missing volume as a backup
// selector, which is exactly the situation a restore exists for.
// ---------------------------------------------------------------------------

func TestLooksLikeArchiveSelector(t *testing.T) {
	selectors := []string{
		"web-20260807-120000.tar.gz", "backup.tar.zst", "backup.tar.lz4",
		"*.tar.gz", "web-2026*", "/abs/path/archive.tar.xz", "sub/dir/a.tar.gz", "a[0-9].tar.gz",
	}
	for _, s := range selectors {
		if !looksLikeArchiveSelector(s) {
			t.Errorf("%q should be recognised as a backup selector", s)
		}
	}
	volumes := []string{"db", "web", "pg_data", "myproj_data", "cache-1", "a.b", "volume_2026"}
	for _, v := range volumes {
		if looksLikeArchiveSelector(v) {
			t.Errorf("%q should be treated as a volume name", v)
		}
	}
}

func TestRestoreMultipleVolumesWhenOneIsMissing(t *testing.T) {
	store := newCLIStore(t)
	seedCLIVolume(t, store, "web", "web-data")
	seedCLIVolume(t, store, "db", "db-data")

	// "db" no longer exists in Docker: the disaster-recovery case.
	recreated := t.TempDir()
	fake := &fakeDockerVolumes{
		volumes: map[string]dockerVolumeInfo{
			"web": {Name: "web", Driver: "local", Mountpoint: store.VolumePath("web")},
		},
		createMountpoints: map[string]string{"db": recreated},
	}
	a := &app{store: store, docker: fake}
	a.cmdRestore([]string{"web", "db"})

	if got, err := os.ReadFile(filepath.Join(store.VolumePath("web"), "data.txt")); err != nil || string(got) != "web-data" {
		t.Fatalf("web not restored: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(recreated, "data.txt")); err != nil || string(got) != "db-data" {
		t.Fatalf("db not restored into the recreated volume: %q err=%v", got, err)
	}
}

func TestRestoreArchiveFlagSelectsTheArchive(t *testing.T) {
	store := newCLIStore(t)
	seedCLIVolume(t, store, "web", "v1")
	if err := os.WriteFile(filepath.Join(store.VolumePath("web"), "data.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	infos, err := store.ListBackups("web")
	if err != nil || len(infos) != 1 {
		t.Fatalf("setup: %+v %v", infos, err)
	}
	a := &app{store: store, docker: dockerForStore(store, "web")}
	a.cmdRestore([]string{"--archive", infos[0].Path, "web"})
	if got, err := os.ReadFile(filepath.Join(store.VolumePath("web"), "data.txt")); err != nil || string(got) != "v1" {
		t.Fatalf("--archive did not select the archive: %q err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// H2: compose-restore could not find bundles written with --timestamp or
// --archive-name.
// ---------------------------------------------------------------------------

func TestBundleBelongsToProject(t *testing.T) {
	cases := map[string]bool{
		"demo-20260808-075843.694163967.tar.gz": true, // default naming
		"demo_08082026_101010.tar.gz":           true, // --timestamp naming
		"demolition-2026.tar.gz":                false,
		"other-2026.tar.gz":                     false,
	}
	for name, want := range cases {
		if got := bundleBelongsToProject(name, "demo"); got != want {
			t.Errorf("bundleBelongsToProject(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLatestBundleFindsTimestampedAndRenamedBundles(t *testing.T) {
	store := newCLIStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{"demo_data": source}
	names := []string{"demo_data"}

	stamped, err := store.BackupBundleSourcesWithOptions(names, sources, store.BundleDir(), "demo",
		dvol.BackupOptions{Timestamp: true, TimestampValue: "08082026_101010"})
	if err != nil {
		t.Fatal(err)
	}
	decoySource, err := store.BackupSourceWithOptions("decoy", source, dvol.BackupOptions{ArchiveName: "decoy"})
	if err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(store.BundleDir(), "demo-decoy.tar.gz")
	if err := os.Rename(decoySource, decoy); err != nil {
		t.Fatal(err)
	}
	got, err := latestBundle(store.BundleDir(), "demo", names)
	if err != nil || got != stamped {
		t.Fatalf("timestamped bundle not found past newer prefixed single-volume decoy: got %q err=%v", got, err)
	}

	// A bundle renamed with --archive-name carries no project prefix: the
	// manifest is the fallback.
	renamed, err := store.BackupBundleSourcesWithOptions(names, sources, store.BundleDir(), "demo",
		dvol.BackupOptions{ArchiveName: "nightly-dump"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stamped); err != nil {
		t.Fatal(err)
	}
	got, err = latestBundle(store.BundleDir(), "demo", names)
	if err != nil || got != renamed {
		t.Fatalf("renamed bundle not found through its manifest: got %q err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// H3: compose-backup silently skipped missing volumes and compose-restore
// created every declared volume before checking the manifest.
// ---------------------------------------------------------------------------

func writeComposeFile(t *testing.T, project string, volumes ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\nvolumes:\n", project)
	for _, v := range volumes {
		fmt.Fprintf(&b, "  %s:\n", v)
	}
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestComposeBackupRefusesMissingVolumeByDefault(t *testing.T) {
	root := t.TempDir()
	store, err := dvol.NewStore(dvol.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	dataMount := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataMount, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := writeComposeFile(t, "proj", "data", "cache")

	_, stderr, code := runChild(t, childSpec{
		Root:    root,
		Volumes: map[string]string{"proj_data": dataMount},
		Command: "compose-backup",
		Args:    []string{"-f", compose},
	})
	if code == 0 {
		t.Fatal("compose-backup must refuse to silently back up a subset")
	}
	if !strings.Contains(stderr, "proj_cache") || !strings.Contains(stderr, "--skip-missing") {
		t.Fatalf("the error must name the missing volume and the opt-in flag: %s", stderr)
	}
	if bundles, err := dvol.ListArchives(store.BundleDir()); err != nil || len(bundles) != 0 {
		t.Fatalf("a refused compose-backup must not leave an archive: %+v err=%v", bundles, err)
	}

	// With the opt-in it succeeds and says what it left out.
	_, stderr, code = runChild(t, childSpec{
		Root:    root,
		Volumes: map[string]string{"proj_data": dataMount},
		Command: "compose-backup",
		Args:    []string{"-f", compose, "--skip-missing"},
	})
	if code != 0 {
		t.Fatalf("--skip-missing must allow the partial backup: %s", stderr)
	}
	if !strings.Contains(stderr, "proj_cache") {
		t.Fatalf("the partial backup must warn about the skipped volume: %s", stderr)
	}
	bundles, err := dvol.ListArchives(store.BundleDir())
	if err != nil || len(bundles) != 1 {
		t.Fatalf("expected one bundle: %+v err=%v", bundles, err)
	}
}

func TestComposeRestoreDoesNotCreateVolumesOnManifestMismatch(t *testing.T) {
	root := t.TempDir()
	store, err := dvol.NewStore(dvol.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	dataMount := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataMount, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, err := store.BackupBundleSourcesWithOptions([]string{"proj_data"},
		map[string]string{"proj_data": dataMount}, store.BundleDir(), "proj", dvol.BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	compose := writeComposeFile(t, "proj", "data", "cache")
	cacheMount := t.TempDir()

	// --strict: the archive does not carry proj_cache, so the restore fails and
	// must not have created it in Docker.
	_, stderr, code := runChild(t, childSpec{
		Root:      root,
		Volumes:   map[string]string{"proj_data": dataMount},
		Creatable: map[string]string{"proj_cache": cacheMount},
		Command:   "compose-restore",
		Args:      []string{"-f", compose, "--strict", archive},
	})
	if code == 0 {
		t.Fatal("--strict must refuse a bundle missing a declared volume")
	}
	if !strings.Contains(stderr, "proj_cache") {
		t.Fatalf("error must name the missing volume: %s", stderr)
	}
	if entries, err := os.ReadDir(cacheMount); err != nil || len(entries) != 0 {
		t.Fatalf("the refused restore touched the volume it should not have created: %v err=%v", entries, err)
	}

	// The default tolerates the gap, restores what is there and warns.
	stdout, stderr, code := runChild(t, childSpec{
		Root:      root,
		Volumes:   map[string]string{"proj_data": dataMount},
		Creatable: map[string]string{"proj_cache": cacheMount},
		Command:   "compose-restore",
		Args:      []string{"-f", compose, archive},
	})
	if code != 0 {
		t.Fatalf("default compose-restore should tolerate an absent volume: %s", stderr)
	}
	if !strings.Contains(stderr, "proj_cache") {
		t.Fatalf("the operator must be warned about the untouched volume: %s", stderr)
	}
	if !strings.Contains(stdout, "restored compose project proj") {
		t.Fatalf("unexpected output: %s", stdout)
	}
	if entries, err := os.ReadDir(cacheMount); err != nil || len(entries) != 0 {
		t.Fatalf("a volume absent from the bundle must stay untouched: %v err=%v", entries, err)
	}
}

// ---------------------------------------------------------------------------
// H5: nothing stopped a restore into a volume a running container had mounted.
// ---------------------------------------------------------------------------

func TestRestoreRefusesVolumeMountedByRunningContainer(t *testing.T) {
	root := t.TempDir()
	store, err := dvol.NewStore(dvol.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	mount := seedCLIVolume(t, store, "web", "archived")
	if err := os.WriteFile(filepath.Join(mount, "data.txt"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runChild(t, childSpec{
		Root:    root,
		Volumes: map[string]string{"web": mount},
		InUse:   map[string][]string{"web": {"app-1", "worker-2"}},
		Command: "restore",
		Args:    []string{"web"},
	})
	if code == 0 {
		t.Fatal("restoring into a mounted volume must be refused")
	}
	for _, want := range []string{"app-1", "worker-2", "--force"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the refusal must mention %q: %s", want, stderr)
		}
	}
	if got, err := os.ReadFile(filepath.Join(mount, "data.txt")); err != nil || string(got) != "live" {
		t.Fatalf("the refused restore modified the volume: %q err=%v", got, err)
	}

	_, stderr, code = runChild(t, childSpec{
		Root:    root,
		Volumes: map[string]string{"web": mount},
		InUse:   map[string][]string{"web": {"app-1"}},
		Command: "restore",
		Args:    []string{"--force", "web"},
	})
	if code != 0 {
		t.Fatalf("--force must allow the restore: %s", stderr)
	}
	if got, err := os.ReadFile(filepath.Join(mount, "data.txt")); err != nil || string(got) != "archived" {
		t.Fatalf("--force restore did not run: %q err=%v", got, err)
	}
	if !strings.Contains(stderr, "mounted by running containers") {
		t.Fatalf("--force must still warn: %s", stderr)
	}
}

func TestRestoreRefusesWhenContainerUsageCannotBeChecked(t *testing.T) {
	root := t.TempDir()
	store, err := dvol.NewStore(dvol.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	mount := seedCLIVolume(t, store, "web", "archived")
	if err := os.WriteFile(filepath.Join(mount, "data.txt"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runChild(t, childSpec{
		Root: root, Volumes: map[string]string{"web": mount}, InUseErr: "Docker unavailable",
		Command: "restore", Args: []string{"web"},
	})
	if code == 0 || !strings.Contains(stderr, "--force") || !strings.Contains(stderr, "Docker unavailable") {
		t.Fatalf("an unverifiable idle state must fail closed: code=%d stderr=%s", code, stderr)
	}
	if got, err := os.ReadFile(filepath.Join(mount, "data.txt")); err != nil || string(got) != "live" {
		t.Fatalf("failed-closed restore modified the volume: %q err=%v", got, err)
	}

	_, stderr, code = runChild(t, childSpec{
		Root: root, Volumes: map[string]string{"web": mount}, InUseErr: "Docker unavailable",
		Command: "restore", Args: []string{"--force", "web"},
	})
	if code != 0 {
		t.Fatalf("--force must explicitly override the failed check: %s", stderr)
	}
	if got, err := os.ReadFile(filepath.Join(mount, "data.txt")); err != nil || string(got) != "archived" {
		t.Fatalf("forced restore did not run: %q err=%v", got, err)
	}
}

func TestBundleRestoreRefusesVolumeInUse(t *testing.T) {
	root := t.TempDir()
	store, err := dvol.NewStore(dvol.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	appMount, dbMount := t.TempDir(), t.TempDir()
	for _, m := range []string{appMount, dbMount} {
		if err := os.WriteFile(filepath.Join(m, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := store.BackupBundleSourcesWithOptions([]string{"app", "db"},
		map[string]string{"app": appMount, "db": dbMount}, store.BundleDir(), "proj", dvol.BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runChild(t, childSpec{
		Root:    root,
		Volumes: map[string]string{"app": appMount, "db": dbMount},
		InUse:   map[string][]string{"db": {"db-1"}},
		Command: "restore",
		Args:    []string{"--bundle", "app", "db", archive},
	})
	if code == 0 {
		t.Fatal("a bundle restore must refuse a volume in use")
	}
	if !strings.Contains(stderr, "db-1") {
		t.Fatalf("the refusal must name the container: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// verify
// ---------------------------------------------------------------------------

func TestCmdVerifyReportsAndFails(t *testing.T) {
	root := t.TempDir()
	store, err := dvol.NewStore(dvol.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	mount := seedCLIVolume(t, store, "web", "payload")
	_ = mount

	stdout, stderr, code := runChild(t, childSpec{
		Root:    root,
		Volumes: map[string]string{"web": store.VolumePath("web")},
		Command: "verify",
		Args:    []string{"web"},
	})
	if code != 0 {
		t.Fatalf("verify of a healthy archive failed: %s", stderr)
	}
	if !strings.Contains(stdout, "OK ") || !strings.Contains(stdout, "entries") {
		t.Fatalf("unexpected verify output: %s", stdout)
	}

	infos, err := store.ListBackups("web")
	if err != nil || len(infos) != 1 {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(infos[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(infos[0].Path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runChild(t, childSpec{
		Root:    root,
		Volumes: map[string]string{"web": store.VolumePath("web")},
		Command: "verify",
		Args:    []string{"web"},
	})
	if code == 0 {
		t.Fatal("verify must fail on a corrupted archive")
	}
	if !strings.Contains(stderr, "FAIL") {
		t.Fatalf("verify must report the failing archive: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Misc CLI guards
// ---------------------------------------------------------------------------

func TestRestoreRejectsDuplicateVolumes(t *testing.T) {
	root := t.TempDir()
	if _, err := dvol.NewStore(dvol.Config{Root: root}); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runChild(t, childSpec{
		Root:    root,
		Command: "restore",
		Args:    []string{"web,web"},
	})
	if code == 0 || !strings.Contains(stderr, "listed twice") {
		t.Fatalf("duplicate volumes must be refused: code=%d %s", code, stderr)
	}
}

func TestBackupStdoutRejectsIncompatibleFlags(t *testing.T) {
	root := t.TempDir()
	if _, err := dvol.NewStore(dvol.Config{Root: root}); err != nil {
		t.Fatal(err)
	}
	mount := t.TempDir()
	for _, args := range [][]string{
		{"--stdout", "--timestamp", "web"},
		{"--stdout", "--overwrite", "web"},
		{"--stdout", "web", "other"},
	} {
		_, stderr, code := runChild(t, childSpec{
			Root:    root,
			Volumes: map[string]string{"web": mount, "other": mount},
			Command: "backup",
			Args:    args,
		})
		if code == 0 {
			t.Fatalf("%v should have been refused (%s)", args, stderr)
		}
	}
}

func TestCLIRejectsArgumentsItWouldOtherwiseIgnore(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{command: "compose-backup", args: []string{"unexpected"}},
		{command: "compose-restore", args: []string{"one.tar.gz", "two.tar.gz"}},
		{command: "verify", args: []string{"web", "one.tar.gz", "ignored.tar.gz"}},
		{command: "verify", args: []string{"--archive", "one.tar.gz", "ignored"}},
	} {
		_, stderr, code := runChild(t, childSpec{Root: root, Command: tc.command, Args: tc.args})
		if code == 0 {
			t.Fatalf("%s %v silently accepted extra arguments: %s", tc.command, tc.args, stderr)
		}
	}
}

func TestResolveComposeFileRejectsSilentlyIgnoredOverrides(t *testing.T) {
	if _, err := resolveComposeFile([]string{"compose.yaml", "override.yaml"}); err == nil || !strings.Contains(err.Error(), "multiple -f") {
		t.Fatalf("multiple compose files must be rejected explicitly, got %v", err)
	}
}
