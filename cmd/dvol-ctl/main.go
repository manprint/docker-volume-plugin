// Command dvol-ctl is the host-side CLI for managing dvol volumes and
// running backup/restore operations directly against the backing store.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/fabio/dvol/pkg/dvol"
)

func main() {
	root := flag.String("root", dvol.DefaultRoot, "backing store directory")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	cmd, args := flag.Arg(0), flag.Args()[1:]

	store, err := dvol.NewStore(dvol.Config{Root: *root})
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "list", "ls":
		runList(store)
	case "create":
		if len(args) < 1 {
			fatal("usage: dvol-ctl create <volume>")
		}
		for _, name := range args {
			if err := store.Create(name, nil); err != nil {
				fatal(err)
			}
			fmt.Printf("created %s\n", name)
		}
	case "rm", "remove":
		if len(args) < 1 {
			fatal("usage: dvol-ctl rm <volume>")
		}
		for _, name := range args {
			if err := store.Remove(name); err != nil {
				fatal(err)
			}
			fmt.Printf("removed %s\n", name)
		}
	case "backup":
		if len(args) < 1 {
			fatal("usage: dvol-ctl backup <volume>")
		}
		for _, name := range args {
			p, err := store.Backup(name)
			if err != nil {
				fatal(err)
			}
			fmt.Printf("backup of %s -> %s\n", name, p)
		}
	case "restore":
		if len(args) < 2 {
			fatal("usage: dvol-ctl restore <volume> <backup|path>")
		}
		runRestore(store, args[0], args[1])
	case "backups":
		if len(args) < 1 {
			fatal("usage: dvol-ctl backups <volume>")
		}
		runBackups(store, args[0])
	default:
		usage()
		os.Exit(2)
	}
}

func runList(store *dvol.Store) {
	vols, err := store.List()
	if err != nil {
		fatal(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCREATED\tSIZE\tMOUNTPOINT")
	for _, v := range vols {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.Name,
			v.CreatedAt.Format("2006-01-02 15:04"), humanSize(v.Size), store.VolumePath(v.Name))
	}
	tw.Flush()
}

func runRestore(store *dvol.Store, name, backupArg string) {
	backupPath, err := resolveBackup(store, name, backupArg)
	if err != nil {
		fatal(err)
	}
	if err := store.Restore(name, backupPath); err != nil {
		fatal(err)
	}
	fmt.Printf("restored %s from %s\n", name, backupPath)
}

func runBackups(store *dvol.Store, name string) {
	infos, err := store.ListBackups(name)
	if err != nil {
		fatal(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BACKUP\tSIZE\tCREATED")
	for _, b := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, humanSize(b.Size),
			b.CreatedAt.Format("2006-01-02 15:04"))
	}
	tw.Flush()
}

// resolveBackup returns the archive path: an exact/glob path, or a filename
// (exact or glob) resolved against backups/<volume>/.
func resolveBackup(store *dvol.Store, name, arg string) (string, error) {
	return store.ResolveBackup(name, arg)
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

func usage() {
	fmt.Fprintf(os.Stderr, `dvol-ctl - backup/restore CLI for the dvol docker volume plugin

Usage:
  dvol-ctl [--root /var/lib/dvol] <command> [args]

Commands:
  list                     list volumes
  create <volume>...       create one or more volumes
  rm <volume>...           remove volumes
  backup <volume>...       backup volumes to backups/<name>/
  restore <volume> <b|path>  restore a volume from a backup
  backups <volume>         list backups of a volume

`)
}

func fatal(v any) {
	fmt.Fprintln(os.Stderr, "dvol-ctl:", v)
	os.Exit(1)
}
