package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wilminator/vm-objects/internal/source"
)

// cmdZFSCleanup lists, and with --apply removes, the ZFS snapshots and
// clones that a vmobj run left behind (it was killed, or the host lost
// power). Like qvs-cleanup it only lists unless told to apply.
func cmdZFSCleanup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("zfs-cleanup", flag.ExitOnError)
	dataset := fs.String("dataset", "", "the dataset holding the VM zvols, searched recursively (e.g. fast/vms)")
	olderThan := fs.Duration("older-than", 24*time.Hour, "leave alone anything newer than this (a running backup owns its snapshot; a big disk can take hours)")
	apply := fs.Bool("apply", false, "remove them; without it, only list them")
	fs.Parse(args)
	if err := require(fs, "dataset"); err != nil {
		return err
	}
	if *olderThan < time.Hour {
		return fmt.Errorf("zfs-cleanup: --older-than %s is too short: a running backup's snapshot could be removed under it (the minimum is 1h)", *olderThan)
	}
	found, tooNew, err := source.FindLeftovers(ctx, source.ExecZFS{}, *dataset, time.Now(), *olderThan)
	if err != nil {
		return err
	}
	if tooNew > 0 {
		fmt.Printf("%d newer than %s left alone (a backup may be running).\n", tooNew, *olderThan)
	}
	if len(found) == 0 {
		fmt.Printf("No leftover vmobj snapshots or clones under %s.\n", *dataset)
		return nil
	}
	for _, l := range found {
		kind := "snapshot"
		if l.Clone {
			kind = "clone"
		}
		verb := "Would remove"
		if *apply {
			verb = "Removing"
		}
		fmt.Printf("%s %s %s, taken %s\n", verb, kind, l.Name, l.Taken.Local().Format("2006-01-02 15:04"))
	}
	if !*apply {
		fmt.Printf("%d to remove; nothing was changed. Run again with --apply.\n", len(found))
		fmt.Fprintln(os.Stderr, "Note: a snapshot you kept on purpose with --keep-snapshot has the same name and is listed too.")
		return nil
	}
	removed, err := source.DestroyLeftovers(ctx, source.ExecZFS{}, found)
	fmt.Printf("Removed %d of %d.\n", len(removed), len(found))
	return err
}
