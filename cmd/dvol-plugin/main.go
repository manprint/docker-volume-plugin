// Command dvol-plugin is the volume driver daemon. It is the plugin entrypoint
// and speaks the docker volume driver protocol over a unix socket.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"sync"

	"github.com/docker/go-plugins-helpers/volume"

	"github.com/fabio/dvol/pkg/dvol"
)

const socketName = "dvol"

func main() {
	root := flag.String("root", dvol.DefaultRoot, "backing store directory")
	flag.Parse()

	store, err := dvol.NewStore(dvol.Config{Root: *root})
	if err != nil {
		log.Fatalf("dvol: %v", err)
	}
	handler := volume.NewHandler(newDriver(store))
	log.Printf("dvol: listening on %s.sock (root=%s)", socketName, *root)
	if err := handler.ServeUnix(socketName, 0); err != nil {
		log.Fatalf("dvol: %v", err)
	}
}

// driver adapts the dvol store to the docker volume driver API.
type driver struct {
	store *dvol.Store

	mu      sync.Mutex
	mounts  map[string]int // volume name -> active mount count
	mounted map[string]bool
}

func newDriver(store *dvol.Store) *driver {
	return &driver{
		store:   store,
		mounts:  map[string]int{},
		mounted: map[string]bool{},
	}
}

func (d *driver) Create(req *volume.CreateRequest) error {
	if err := d.store.Create(req.Name, req.Options); err != nil {
		if errors.Is(err, dvol.ErrExists) {
			return nil // idempotent: docker may retry
		}
		return err
	}
	log.Printf("created volume %q", req.Name)
	return nil
}

func (d *driver) Remove(req *volume.RemoveRequest) error {
	d.mu.Lock()
	if d.mounts[req.Name] > 0 {
		d.mu.Unlock()
		return fmt.Errorf("volume %q is mounted", req.Name)
	}
	d.mu.Unlock()
	return d.store.Remove(req.Name)
}

func (d *driver) Path(req *volume.PathRequest) (*volume.PathResponse, error) {
	if _, err := d.store.Get(req.Name); err != nil {
		return nil, err
	}
	return &volume.PathResponse{Mountpoint: d.store.VolumePath(req.Name)}, nil
}

func (d *driver) Mount(req *volume.MountRequest) (*volume.MountResponse, error) {
	if _, err := d.store.Get(req.Name); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mounts[req.Name]++
	return &volume.MountResponse{Mountpoint: d.store.VolumePath(req.Name)}, nil
}

func (d *driver) Unmount(req *volume.UnmountRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mounts[req.Name] > 0 {
		d.mounts[req.Name]--
		if d.mounts[req.Name] == 0 {
			delete(d.mounts, req.Name)
		}
	}
	return nil
}

func (d *driver) Get(req *volume.GetRequest) (*volume.GetResponse, error) {
	v, err := d.store.Get(req.Name)
	if err != nil {
		return nil, err
	}
	return &volume.GetResponse{Volume: toHelper(v, d.store.VolumePath(v.Name))}, nil
}

func (d *driver) List() (*volume.ListResponse, error) {
	vols, err := d.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]*volume.Volume, 0, len(vols))
	for _, v := range vols {
		out = append(out, toHelper(v, d.store.VolumePath(v.Name)))
	}
	return &volume.ListResponse{Volumes: out}, nil
}

func (d *driver) Capabilities() *volume.CapabilitiesResponse {
	return &volume.CapabilitiesResponse{
		Capabilities: volume.Capability{Scope: "local"},
	}
}

func toHelper(v *dvol.Volume, mountpoint string) *volume.Volume {
	return &volume.Volume{
		Name:       v.Name,
		Mountpoint: mountpoint,
		CreatedAt:  v.CreatedAt.Format("2006-01-02T15:04:05Z"),
		Status: map[string]interface{}{
			"size_bytes": v.Size,
		},
	}
}
