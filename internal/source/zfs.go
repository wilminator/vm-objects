// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ZFS backs up a zvol from a snapshot taken at the start of the run, so
// the backup is one consistent point in time even while the VM runs.
//
// The snapshot is read through a temporary read-only clone, whose device
// node appears under /dev/zvol. That leaves the zvol's own properties
// alone (the other way, snapdev=visible, would turn every snapshot of the
// zvol into a device). On Close the clone and the snapshot are destroyed.
// They are named "<zvol>@vmobj-<time>" and "<zvol>-vmobj-<time>", and
// vmobj never destroys anything not named that way.
type ZFS struct {
	*Raw
	z        ZFSRunner
	snapshot string
	clone    string
	cloned   bool // this run created the clone, so it may destroy it
	keep     bool
}

// ZFSRunner runs zfs commands. The real one runs the zfs program; tests
// use a fake.
type ZFSRunner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// ExecZFS runs the zfs command line tool.
type ExecZFS struct{ Path string }

// Run runs zfs with args and returns its standard output.
func (e ExecZFS) Run(ctx context.Context, args ...string) (string, error) {
	bin := e.Path
	if bin == "" {
		bin = "zfs"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("zfs %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// ZFSOptions configure OpenZFS.
type ZFSOptions struct {
	Runner ZFSRunner // default: ExecZFS
	DevDir string    // default: /dev/zvol
	// KeepSnapshot leaves the snapshot in place after the backup (the
	// clone is always removed).
	KeepSnapshot bool
	Now          func() time.Time
	// DeviceWait is how long to wait for the clone's device node.
	DeviceWait time.Duration
	// Snapshot, when set, is an existing snapshot of the zvol to read
	// (from SnapshotZvols). OpenZFS then takes none of its own, but still
	// destroys this one on Close unless KeepSnapshot is set.
	Snapshot string
	// Existing, when set, is one of the zvol's own snapshots to read
	// ("pool/zvol@name"), such as a VM snapshot taken before an update.
	// It is read through a clone like any other, and never destroyed:
	// KeepSnapshot is implied.
	Existing string
}

// SnapshotZvols snapshots several zvols in one zfs command, which ZFS
// does atomically: every disk of a VM is captured at the same instant.
// It returns the snapshot name per zvol.
func SnapshotZvols(ctx context.Context, r ZFSRunner, now time.Time, datasets ...string) (map[string]string, error) {
	if r == nil {
		r = ExecZFS{}
	}
	stamp := now.UTC().Format("20060102T150405Z")
	names := map[string]string{}
	args := []string{"snapshot"}
	for _, ds := range datasets {
		if err := checkZvolName(ds); err != nil {
			return nil, err
		}
		names[ds] = ds + "@vmobj-" + stamp
		args = append(args, names[ds])
	}
	if _, err := r.Run(ctx, args...); err != nil {
		return nil, err
	}
	return names, nil
}

func checkZvolName(dataset string) error {
	if !zfsName.MatchString(dataset) || strings.Contains(dataset, "//") || strings.HasSuffix(dataset, "/") ||
		strings.Contains(dataset, "..") || !strings.Contains(dataset, "/") {
		return fmt.Errorf("%q is not a zvol name like pool/vms/win11", dataset)
	}
	return nil
}

var zfsName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*$`)

// OpenZFS snapshots a zvol and opens the snapshot for reading.
func OpenZFS(ctx context.Context, dataset string, o ZFSOptions) (*ZFS, error) {
	if err := checkZvolName(dataset); err != nil {
		return nil, err
	}
	if o.Runner == nil {
		o.Runner = ExecZFS{}
	}
	if o.DevDir == "" {
		o.DevDir = "/dev/zvol"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.DeviceWait == 0 {
		o.DeviceWait = 30 * time.Second
	}
	typ, err := o.Runner.Run(ctx, "get", "-H", "-o", "value", "type", dataset)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(typ) != "volume" {
		return nil, fmt.Errorf("%s is a ZFS %s, not a zvol", dataset, strings.TrimSpace(typ))
	}

	stamp := o.Now().UTC().Format("20060102T150405Z")
	z := &ZFS{z: o.Runner, snapshot: dataset + "@vmobj-" + stamp, clone: dataset + "-vmobj-" + stamp, keep: o.KeepSnapshot}
	if o.Existing != "" {
		if o.Snapshot != "" {
			return nil, errors.New("give an existing snapshot or a vmobj one, not both")
		}
		snap, ok := strings.CutPrefix(o.Existing, dataset+"@")
		if !ok {
			return nil, fmt.Errorf("%s is not a snapshot of %s", o.Existing, dataset)
		}
		if err := CheckZFSSnapshotName(snap); err != nil {
			return nil, err
		}
		if _, err := o.Runner.Run(ctx, "list", "-H", "-o", "name", "-t", "snapshot", o.Existing); err != nil {
			return nil, err
		}
		// The clone's name carries the snapshot's, so reading several
		// snapshots in one second never reuses a name (or a stale
		// /dev/zvol link).
		z.snapshot, z.clone, z.keep = o.Existing, dataset+"-vmobj-"+stamp+"-"+snap, true
	} else if o.Snapshot != "" {
		// Only snapshots that vmobj names may be adopted, so Close can
		// never destroy someone else's snapshot.
		snapStamp, ok := strings.CutPrefix(o.Snapshot, dataset+"@vmobj-")
		if !ok || len(snapStamp) != len(stamp) {
			return nil, fmt.Errorf("%s is not a vmobj snapshot of %s", o.Snapshot, dataset)
		}
		z.snapshot, z.clone = o.Snapshot, dataset+"-vmobj-"+snapStamp
	}
	if o.Existing == "" && o.Snapshot == "" {
		if _, err := o.Runner.Run(ctx, "snapshot", z.snapshot); err != nil {
			return nil, err
		}
	}
	// From here on, undo on failure.
	if _, err := o.Runner.Run(ctx, "clone", "-o", "readonly=on", z.snapshot, z.clone); err != nil {
		z.cleanup(context.WithoutCancel(ctx))
		return nil, err
	}
	z.cloned = true
	dev := filepath.Join(o.DevDir, z.clone)
	if err := waitFor(ctx, dev, o.DeviceWait); err != nil {
		z.cleanup(context.WithoutCancel(ctx))
		return nil, err
	}
	raw, err := OpenRaw(dev)
	if err != nil {
		z.cleanup(context.WithoutCancel(ctx))
		return nil, err
	}
	z.Raw = raw
	return z, nil
}

// Snapshot names the snapshot the backup was read from.
func (z *ZFS) Snapshot() string { return z.snapshot }

// waitFor waits for udev to create a device node.
func waitFor(ctx context.Context, path string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within %s", path, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Close closes the device and removes the clone and (unless kept) the
// snapshot. Cleanup runs even if the backup was cancelled.
func (z *ZFS) Close() error {
	var err error
	if z.Raw != nil {
		err = z.Raw.Close()
	}
	return errors.Join(err, z.cleanup(context.Background()))
}

func (z *ZFS) cleanup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var errs []error
	// Destroy the clone first: a snapshot with a clone can't be destroyed.
	// Only a clone this run made: a failed clone may mean the name was
	// already taken by something that isn't ours.
	if z.cloned {
		if _, err := z.z.Run(ctx, "destroy", z.clone); err != nil {
			errs = append(errs, err)
		}
	}
	if !z.keep {
		if _, err := z.z.Run(ctx, "destroy", z.snapshot); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ZFSSnapshot is one of a zvol's own named snapshots.
type ZFSSnapshot struct {
	Name string // the part after "@"
	// Created is when the snapshot was first taken: its vmobj:created
	// property when vmobj recreated it from a backup (ZFS gives a
	// recreated snapshot today's date), otherwise ZFS's creation time.
	Created     time.Time
	Description string // the vmobj:description property, if set
}

// ListZFSSnapshots lists a zvol's snapshots in the order ZFS took them,
// leaving out the ones vmobj takes for its own runs ("@vmobj-...").
func ListZFSSnapshots(ctx context.Context, r ZFSRunner, dataset string) ([]ZFSSnapshot, error) {
	if err := checkZvolName(dataset); err != nil {
		return nil, err
	}
	if r == nil {
		r = ExecZFS{}
	}
	out, err := r.Run(ctx, "list", "-H", "-p", "-t", "snapshot", "-d", "1", "-s", "creation",
		"-o", "name,creation,vmobj:created,vmobj:description", dataset)
	if err != nil {
		return nil, err
	}
	var snaps []ZFSSnapshot
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected zfs list output: %q", line)
		}
		name, ok := strings.CutPrefix(f[0], dataset+"@")
		if !ok {
			return nil, fmt.Errorf("unexpected snapshot %q in zfs list output", f[0])
		}
		if strings.HasPrefix(name, "vmobj-") {
			continue
		}
		secs, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: creation %q: %w", f[0], f[1], err)
		}
		sn := ZFSSnapshot{Name: name, Created: time.Unix(secs, 0).UTC()}
		if f[2] != "-" && f[2] != "" {
			t, err := time.Parse(time.RFC3339, f[2])
			if err != nil {
				return nil, fmt.Errorf("snapshot %s: vmobj:created %q: %w", f[0], f[2], err)
			}
			sn.Created = t.UTC()
		}
		if f[3] != "-" {
			sn.Description = f[3]
		}
		snaps = append(snaps, sn)
	}
	return snaps, nil
}

var zfsSnapName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)

// CheckZFSSnapshotName allows the characters ZFS allows in a snapshot
// name, apart from the space (which shells and scripts handle badly).
func CheckZFSSnapshotName(name string) error {
	if !zfsSnapName.MatchString(name) {
		return fmt.Errorf("%q is not a usable ZFS snapshot name (letters, digits, '_', '.', ':' and '-')", name)
	}
	return nil
}

// ZFSSnapshotName turns any snapshot name (a QNAP one may hold spaces)
// into one ZFS accepts, replacing other characters with '_'. The original
// is kept in the vmobj:name property by whoever recreates the snapshot.
func ZFSSnapshotName(name string) string {
	b := []byte(name)
	for i, c := range b {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' || c == '-'
		if !ok {
			b[i] = '_'
		}
	}
	if len(b) > 200 {
		b = b[:200]
	}
	switch out := string(b); {
	case out == "":
		return "unnamed"
	case strings.HasPrefix(out, "vmobj-"):
		return "_" + out // vmobj- names are vmobj's own temporary snapshots
	default:
		return out
	}
}
