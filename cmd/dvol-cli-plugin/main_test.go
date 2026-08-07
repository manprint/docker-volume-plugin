package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fabio/dvol/pkg/dvol"
)

func newCLIStore(t *testing.T) *dvol.Store {
	t.Helper()
	s, err := dvol.NewStore(dvol.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeDockerVolumes struct {
	volumes           map[string]dockerVolumeInfo
	createMountpoints map[string]string
}

func dockerForStore(store *dvol.Store, names ...string) *fakeDockerVolumes {
	volumes := make(map[string]dockerVolumeInfo, len(names))
	for _, name := range names {
		volumes[name] = dockerVolumeInfo{Name: name, Driver: "local", Mountpoint: store.VolumePath(name)}
	}
	return &fakeDockerVolumes{volumes: volumes, createMountpoints: map[string]string{}}
}

func (f *fakeDockerVolumes) List() ([]dockerVolumeInfo, error) {
	result := make([]dockerVolumeInfo, 0, len(f.volumes))
	for _, volume := range f.volumes {
		result = append(result, volume)
	}
	return result, nil
}

func (f *fakeDockerVolumes) Inspect(name string) (dockerVolumeInfo, error) {
	volume, ok := f.volumes[name]
	if !ok {
		return dockerVolumeInfo{}, errDockerVolumeNotFound
	}
	return volume, nil
}

func (f *fakeDockerVolumes) Create(name string) (dockerVolumeInfo, error) {
	if volume, ok := f.volumes[name]; ok {
		return volume, nil
	}
	if mountpoint, ok := f.createMountpoints[name]; ok {
		volume := dockerVolumeInfo{Name: name, Driver: "local", Mountpoint: mountpoint}
		f.volumes[name] = volume
		return volume, nil
	}
	return dockerVolumeInfo{}, errDockerVolumeNotFound
}

func TestDockerVolumeMountpointBackupAndRestore(t *testing.T) {
	store := newCLIStore(t)
	mountpoint := t.TempDir()
	if err := os.WriteFile(filepath.Join(mountpoint, "legacy.txt"), []byte("legacy-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDockerVolumes{volumes: map[string]dockerVolumeInfo{
		"legacy": {Name: "legacy", Driver: "local", Mountpoint: mountpoint},
	}, createMountpoints: map[string]string{}}
	a := &app{store: store, docker: fake}

	source, err := a.volumeSource("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if source != mountpoint {
		t.Fatalf("unexpected Docker source: %q", source)
	}
	archive, err := store.BackupSourceWithOptions("legacy", source, dvol.BackupOptions{})
	if err != nil {
		t.Fatalf("BackupSourceWithOptions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, "changed.txt"), []byte("must disappear"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreVolume("legacy", archive); err != nil {
		t.Fatalf("restoreVolume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(mountpoint, "legacy.txt"))
	if err != nil || string(got) != "legacy-data" {
		t.Fatalf("legacy volume data wrong: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, "changed.txt")); !os.IsNotExist(err) {
		t.Fatalf("changed Docker volume data should have been replaced")
	}
}

func TestVolumeSourceDoesNotUseArchiveRepositoryAsDockerVolume(t *testing.T) {
	store := newCLIStore(t)
	if err := store.Create("orphan", nil); err != nil {
		t.Fatal(err)
	}
	a := &app{store: store, docker: &fakeDockerVolumes{
		volumes:           map[string]dockerVolumeInfo{},
		createMountpoints: map[string]string{},
	}}
	if _, err := a.volumeSource("orphan"); !errors.Is(err, errDockerVolumeNotFound) {
		t.Fatalf("expected Docker lookup failure, got %v", err)
	}
}

func TestRestoreCreatesMissingDockerVolume(t *testing.T) {
	store := newCLIStore(t)
	if err := store.Create("seed", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.VolumePath("seed"), "data.txt"), []byte("seed-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, err := store.Backup("seed")
	if err != nil {
		t.Fatal(err)
	}

	mountpoint := t.TempDir()
	fake := &fakeDockerVolumes{
		volumes:           map[string]dockerVolumeInfo{},
		createMountpoints: map[string]string{"new-local": mountpoint},
	}
	a := &app{store: store, docker: fake}
	if err := a.restoreVolume("new-local", archive); err != nil {
		t.Fatalf("restoreVolume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(mountpoint, "data.txt"))
	if err != nil || string(got) != "seed-data" {
		t.Fatalf("new Docker volume data wrong: %q err=%v", got, err)
	}
}

// TestCmdRestoreSpecificBackup pins the restore-with-selector path:
// `docker dvol restore <vol> <backup>` must restore the requested archive,
// not the latest one (regression: args were expanded as volume names).
func TestCmdRestoreSpecificBackup(t *testing.T) {
	store := newCLIStore(t)
	if err := store.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	write := func(content string) string {
		if err := os.WriteFile(filepath.Join(store.VolumePath("web"), "data.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := store.Backup("web")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := write("v1")
	write("v2")

	a := &app{store: store, docker: dockerForStore(store, "web")}
	a.cmdRestore([]string{"web", filepath.Base(first)})

	got, err := os.ReadFile(filepath.Join(store.VolumePath("web"), "data.txt"))
	if err != nil || string(got) != "v1" {
		t.Fatalf("expected v1 restored, got %q err=%v", got, err)
	}
}

// TestCmdRestoreGlobSelector: `restore <vol> <glob>` resolves inside backups/<vol>/.
func TestCmdRestoreGlobSelector(t *testing.T) {
	store := newCLIStore(t)
	if err := store.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.VolumePath("web"), "data.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Backup("web"); err != nil {
		t.Fatal(err)
	}
	a := &app{store: store, docker: dockerForStore(store, "web")}
	a.cmdRestore([]string{"web", "*.tar.gz"})
	got, err := os.ReadFile(filepath.Join(store.VolumePath("web"), "data.txt"))
	if err != nil || string(got) != "v1" {
		t.Fatalf("expected v1, got %q err=%v", got, err)
	}
}

// TestCmdBackupCompression: --compression xz produces .tar.xz and restores.
func TestCmdBackupCompression(t *testing.T) {
	store := newCLIStore(t)
	if err := store.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.VolumePath("web"), "data.txt"), []byte("xz-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{store: store, docker: dockerForStore(store, "web")}
	a.cmdBackup([]string{"--compression", "xz", "--level", "6", "web"})
	infos, err := store.ListBackups("web")
	if err != nil || len(infos) != 1 {
		t.Fatalf("ListBackups: %+v err=%v", infos, err)
	}
	if got := infos[0].Name; !strings.HasSuffix(got, ".tar.xz") {
		t.Fatalf("expected .tar.xz archive, got %q", got)
	}

	if err := store.Remove("web"); err != nil {
		t.Fatal(err)
	}
	a.cmdRestore([]string{"web"})
	got, err := os.ReadFile(filepath.Join(store.VolumePath("web"), "data.txt"))
	if err != nil || string(got) != "xz-data" {
		t.Fatalf("expected xz-data, got %q err=%v", got, err)
	}
}

func TestCmdBackupArchiveNameAndTimestamp(t *testing.T) {
	store := newCLIStore(t)
	if err := store.Create("mongo_data", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.VolumePath("mongo_data"), "data.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{store: store, docker: dockerForStore(store, "mongo_data")}
	a.cmdBackup([]string{"--compression", "zstd", "--archive-name", "mongodb-vol", "--timestamp", "mongo_data"})

	infos, err := store.ListBackups("mongo_data")
	if err != nil || len(infos) != 1 {
		t.Fatalf("ListBackups: %+v err=%v", infos, err)
	}
	if !regexp.MustCompile(`^mongodb-vol_[0-9]{8}_[0-9]{6}\.tar\.zst$`).MatchString(infos[0].Name) {
		t.Fatalf("unexpected timestamped archive name: %q", infos[0].Name)
	}
}

func TestCmdBackupMultipleMappingsSameDestination(t *testing.T) {
	destination := t.TempDir()
	store, err := dvol.NewStore(dvol.Config{Root: t.TempDir(), BackupsDir: destination})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_data", "filestore_data"} {
		if err := store.Create(name, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store.VolumePath(name), "data.txt"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{store: store, docker: dockerForStore(store, "pg_data", "filestore_data")}
	a.cmdBackup([]string{
		"--archive-name", "pg_data::postgres-odoo",
		"--archive-name", "filestore_data::filestore-odoo",
		"--same-destination",
		"--timestamp",
		"pg_data", "filestore_data",
	})

	pg, err := store.ListBackups("pg_data")
	if err != nil || len(pg) != 1 || filepath.Dir(pg[0].Path) != destination || !strings.HasPrefix(pg[0].Name, "postgres-odoo_") {
		t.Fatalf("pg_data flat backup: %+v err=%v", pg, err)
	}
	files, err := store.ListBackups("filestore_data")
	if err != nil || len(files) != 1 || filepath.Dir(files[0].Path) != destination || !strings.HasPrefix(files[0].Name, "filestore-odoo_") {
		t.Fatalf("filestore_data flat backup: %+v err=%v", files, err)
	}
	pgStamp := strings.TrimPrefix(strings.TrimSuffix(pg[0].Name, ".tar.gz"), "postgres-odoo_")
	fileStamp := strings.TrimPrefix(strings.TrimSuffix(files[0].Name, ".tar.gz"), "filestore-odoo_")
	if pgStamp != fileStamp {
		t.Fatalf("archives did not share one timestamp: %q vs %q", pgStamp, fileStamp)
	}
}

func TestExtractGlobalFlagsDestination(t *testing.T) {
	root, destination, args := extractGlobalFlags([]string{
		"backup", "--destination", "/home/ubuntu/dump-mongo-vol", "mongo_data",
	})
	if root != "" || destination != "/home/ubuntu/dump-mongo-vol" {
		t.Fatalf("unexpected global flags: root=%q destination=%q", root, destination)
	}
	if got := strings.Join(args, " "); got != "backup mongo_data" {
		t.Fatalf("unexpected remaining args: %q", got)
	}
}

func TestParseArchiveNames(t *testing.T) {
	got, err := parseArchiveNames([]string{
		"pg_data::postgres-odoo",
		"filestore_data::filestore-odoo",
	}, []string{"pg_data", "filestore_data"})
	if err != nil {
		t.Fatal(err)
	}
	if got["pg_data"] != "postgres-odoo" || got["filestore_data"] != "filestore-odoo" {
		t.Fatalf("unexpected archive mappings: %#v", got)
	}

	bad := [][]string{
		{"pg_data::one", "pg_data::two"},
		{"unknown::archive"},
		{"pg_data::one"},
	}
	volumes := [][]string{
		{"pg_data"},
		{"pg_data"},
		{"pg_data", "filestore_data"},
	}
	for i := range bad {
		if _, err := parseArchiveNames(bad[i], volumes[i]); err == nil {
			t.Fatalf("mapping %v unexpectedly succeeded", bad[i])
		}
	}
}

// TestCmdComposeBackupCompression: bundle in zstd with level, restore works.
func TestCmdComposeBackupCompression(t *testing.T) {
	store := newCLIStore(t)
	if err := store.Create("myproj_data", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.VolumePath("myproj_data"), "settings.ini"), []byte("zstd!"), 0o644); err != nil {
		t.Fatal(err)
	}
	cf := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(cf, []byte("name: myproj\nvolumes:\n  data:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{store: store, docker: dockerForStore(store, "myproj_data")}
	a.cmdComposeBackup([]string{"-f", cf, "--compression", "zstd", "--level", "9"})
	bundles, err := store.ListArchives(store.BundleDir())
	if err != nil || len(bundles) != 1 {
		t.Fatalf("bundles: %+v err=%v", bundles, err)
	}
	if got := bundles[0].Name; !strings.HasSuffix(got, ".tar.zst") {
		t.Fatalf("expected .tar.zst bundle, got %q", got)
	}

	if err := store.Remove("myproj_data"); err != nil {
		t.Fatal(err)
	}
	a.cmdComposeRestore([]string{"-f", cf})
	got, err := os.ReadFile(filepath.Join(store.VolumePath("myproj_data"), "settings.ini"))
	if err != nil || string(got) != "zstd!" {
		t.Fatalf("expected zstd! got %q err=%v", got, err)
	}
}
func TestCmdRestoreMultipleVolumes(t *testing.T) {
	store := newCLIStore(t)
	for _, n := range []string{"web", "db"} {
		if err := store.Create(n, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store.VolumePath(n), "data.txt"), []byte("d-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Backup(n); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{store: store, docker: dockerForStore(store, "web", "db")}
	a.cmdRestore([]string{"web", "db"})
	for _, n := range []string{"web", "db"} {
		got, err := os.ReadFile(filepath.Join(store.VolumePath(n), "data.txt"))
		if err != nil || string(got) != "d-"+n {
			t.Fatalf("%s: got %q err=%v", n, got, err)
		}
	}
}
