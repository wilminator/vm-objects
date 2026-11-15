package source

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// leftoverZFS answers the two lists FindLeftovers makes and records every
// command, so a test can see exactly what was destroyed.
type leftoverZFS struct {
	volumes   string // "name\torigin" lines
	snapshots string // names
	ran       [][]string
	failOn    string // destroy of this name fails
}

func (f *leftoverZFS) Run(_ context.Context, args ...string) (string, error) {
	f.ran = append(f.ran, args)
	switch args[0] {
	case "list":
		if args[4] == "volume" {
			return f.volumes, nil
		}
		return f.snapshots, nil
	case "destroy":
		if args[1] == f.failOn {
			return "", errors.New("dataset is busy")
		}
	}
	return "", nil
}

func (f *leftoverZFS) destroyed() []string {
	var out []string
	for _, a := range f.ran {
		if a[0] == "destroy" {
			out = append(out, strings.Join(a[1:], " "))
		}
	}
	return out
}

var leftoverNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

const leftoverVolumes = "" +
	"fast/vms/win11\t-\n" + // the VM's own zvol
	"fast/vms/win11-vmobj-20261110T020000Z\tfast/vms/win11@vmobj-20261110T020000Z\n" + // our clone
	"fast/vms/win11-vmobj-20261111T020000Z-before-update\tfast/vms/win11@before-update\n" + // our clone of a user snapshot
	"fast/vms/win11-vmobj-20261115T110000Z\tfast/vms/win11@vmobj-20261115T110000Z\n" + // an hour ago: maybe running
	"fast/vms/odd-vmobj-20261110T020000Z\t-\n" // looks like ours but is not a clone

const leftoverSnapshots = "" +
	"fast/vms/win11@vmobj-20261110T020000Z\n" + // belongs to the first clone
	"fast/vms/win11@before-update\n" + // the user's: never touched
	"fast/vms/win11@vmobj-20261109T020000Z\n" + // a snapshot with no clone
	"fast/vms/win11@vmobj-20261115T110000Z\n" + // an hour ago
	"fast/vms/win11@vmobj-notastamp\n" +
	"fast/vms/win11@auto-daily-2026-11-10_00-00\n"

func TestFindLeftoversNamesOnlyWhatVmobjMade(t *testing.T) {
	z := &leftoverZFS{volumes: leftoverVolumes, snapshots: leftoverSnapshots}
	found, tooNew, err := FindLeftovers(context.Background(), z, "fast/vms", leftoverNow, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range found {
		kind := "snap"
		if l.Clone {
			kind = "clone"
		}
		got = append(got, kind+" "+l.Name)
	}
	want := []string{
		"clone fast/vms/win11-vmobj-20261110T020000Z",
		"clone fast/vms/win11-vmobj-20261111T020000Z-before-update",
		"snap fast/vms/win11@vmobj-20261110T020000Z",
		"snap fast/vms/win11@vmobj-20261109T020000Z",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("found:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if tooNew != 2 { // the young clone and the young snapshot
		t.Fatalf("tooNew = %d, want 2", tooNew)
	}
}

func TestFindLeftoversDateComesFromTheName(t *testing.T) {
	z := &leftoverZFS{snapshots: "fast/vms/win11@vmobj-20261109T020000Z\n"}
	found, _, err := FindLeftovers(context.Background(), z, "fast/vms", leftoverNow, time.Hour)
	if err != nil || len(found) != 1 {
		t.Fatalf("%v %v", found, err)
	}
	if want := time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC); !found[0].Taken.Equal(want) {
		t.Fatalf("taken %v, want %v", found[0].Taken, want)
	}
}

func TestFindLeftoversRefusesOddParents(t *testing.T) {
	for _, p := range []string{"", "fast/vms/", "fast//vms", "fast/../x", "fast/vms@snap", "-r", "fast vms"} {
		z := &leftoverZFS{}
		if _, _, err := FindLeftovers(context.Background(), z, p, leftoverNow, time.Hour); err == nil {
			t.Errorf("accepted parent %q", p)
		}
		if len(z.ran) != 0 {
			t.Errorf("ran zfs for parent %q: %v", p, z.ran)
		}
	}
}

func TestDestroyLeftoversOrderAndNeverRecursive(t *testing.T) {
	z := &leftoverZFS{volumes: leftoverVolumes, snapshots: leftoverSnapshots}
	found, _, _ := FindLeftovers(context.Background(), z, "fast/vms", leftoverNow, 24*time.Hour)
	z.ran = nil
	removed, err := DestroyLeftovers(context.Background(), z, found)
	if err != nil || len(removed) != len(found) {
		t.Fatalf("removed %d of %d: %v", len(removed), len(found), err)
	}
	got := z.destroyed()
	for i, l := range found {
		if got[i] != l.Name {
			t.Fatalf("destroy %d: %q, want %q (clones must go before snapshots)", i, got[i], l.Name)
		}
	}
	for _, a := range z.ran {
		for _, x := range a {
			if x == "-r" || x == "-R" || x == "-f" {
				t.Fatalf("recursive or forced destroy: %v", a)
			}
		}
	}
}

func TestDestroyLeftoversCarriesOnAfterAFailure(t *testing.T) {
	z := &leftoverZFS{volumes: leftoverVolumes, snapshots: leftoverSnapshots, failOn: "fast/vms/win11@vmobj-20261110T020000Z"}
	found, _, _ := FindLeftovers(context.Background(), z, "fast/vms", leftoverNow, 24*time.Hour)
	removed, err := DestroyLeftovers(context.Background(), z, found)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error %v", err)
	}
	if len(removed) != len(found)-1 {
		t.Fatalf("removed %d of %d; one failure should not stop the rest", len(removed), len(found))
	}
}

func TestDestroyLeftoversRefusesANameVmobjDoesNotMake(t *testing.T) {
	z := &leftoverZFS{}
	for _, l := range []Leftover{
		{Name: "fast/vms/win11@before-update"},
		{Name: "fast/vms/win11", Clone: true},
		{Name: "fast/vms/win11@vmobj-20261110T020000Z", Clone: true}, // a snapshot named as a clone
	} {
		if removed, err := DestroyLeftovers(context.Background(), z, []Leftover{l}); err == nil || len(removed) != 0 {
			t.Errorf("destroyed or accepted %+v", l)
		}
	}
	if len(z.destroyed()) != 0 {
		t.Fatalf("destroyed %v", z.destroyed())
	}
}
