package source

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// A Leftover is a snapshot or clone that a vmobj run made and didn't
// remove, typically because it was killed or the host lost power.
type Leftover struct {
	Clone bool      // a clone volume; otherwise a snapshot
	Name  string    // "pool/vms/win11@vmobj-..." or "pool/vms/win11-vmobj-..."
	Taken time.Time // from the stamp in the name (UTC)
}

// vmobj names its own snapshots dataset@vmobj-<stamp> and its clones
// dataset-vmobj-<stamp>, with "-<snapshot>" added when it reads one of the
// zvol's own snapshots. Only exactly these shapes are ever touched.
const stampLayout = "20060102T150405Z"

var (
	leftoverSnap  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*@vmobj-(\d{8}T\d{6}Z)$`)
	leftoverClone = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*-vmobj-(\d{8}T\d{6}Z)(-[A-Za-z0-9_.:-]+)?$`)
)

// FindLeftovers lists vmobj's leftover snapshots and clones under parent (a
// dataset such as "fast/vms", searched recursively), clones first, so
// removing them in order never meets a snapshot that still has a clone of
// ours. Anything taken less than olderThan ago is left out and counted in
// tooNew: it may belong to a backup that is running right now.
func FindLeftovers(ctx context.Context, r ZFSRunner, parent string, now time.Time, olderThan time.Duration) (found []Leftover, tooNew int, err error) {
	if err := checkParent(parent); err != nil {
		return nil, 0, err
	}
	if r == nil {
		r = ExecZFS{}
	}
	cut := now.Add(-olderThan)
	add := func(l Leftover) {
		if l.Taken.After(cut) {
			tooNew++
			return
		}
		found = append(found, l)
	}

	// Clones first: a volume whose name has our shape AND that is a clone
	// (has an origin), so a volume that merely has a similar name is safe.
	out, err := r.Run(ctx, "list", "-H", "-r", "-t", "volume", "-o", "name,origin", parent)
	if err != nil {
		return nil, 0, err
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		name, origin, _ := strings.Cut(line, "\t")
		m := leftoverClone.FindStringSubmatch(name)
		if m == nil || origin == "" || origin == "-" || !strings.Contains(origin, "@") {
			continue
		}
		t, err := time.Parse(stampLayout, m[1])
		if err != nil {
			continue
		}
		add(Leftover{Clone: true, Name: name, Taken: t})
	}

	out, err = r.Run(ctx, "list", "-H", "-r", "-t", "snapshot", "-o", "name", parent)
	if err != nil {
		return nil, 0, err
	}
	for _, name := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		m := leftoverSnap.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		t, err := time.Parse(stampLayout, m[1])
		if err != nil {
			continue
		}
		add(Leftover{Name: name, Taken: t})
	}
	return found, tooNew, nil
}

// DestroyLeftovers removes what FindLeftovers returned, in that order, one
// plain "zfs destroy" each (never -r or -R). It carries on after a failure
// and returns what it removed and the joined errors for what it couldn't,
// such as a snapshot that has a clone that isn't vmobj's.
func DestroyLeftovers(ctx context.Context, r ZFSRunner, found []Leftover) (removed []Leftover, err error) {
	if r == nil {
		r = ExecZFS{}
	}
	var errs []error
	for _, l := range found {
		// The name must still have vmobj's shape: nothing else is destroyed.
		if (l.Clone && !leftoverClone.MatchString(l.Name)) || (!l.Clone && !leftoverSnap.MatchString(l.Name)) {
			errs = append(errs, fmt.Errorf("%q isn't a name vmobj makes; not touched", l.Name))
			continue
		}
		if _, derr := r.Run(ctx, "destroy", l.Name); derr != nil {
			errs = append(errs, derr)
			continue
		}
		removed = append(removed, l)
	}
	return removed, errors.Join(errs...)
}

func checkParent(parent string) error {
	if !zfsName.MatchString(parent) || strings.Contains(parent, "//") || strings.HasSuffix(parent, "/") ||
		strings.Contains(parent, "..") || strings.Contains(parent, "@") {
		return fmt.Errorf("%q is not a dataset name like pool/vms", parent)
	}
	return nil
}
