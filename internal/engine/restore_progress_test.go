// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
)

// progressLog records what Restore reports, checking as it goes that done
// never goes backwards and total stays fixed within a phase.
type progressLog struct {
	t      *testing.T
	phases []Phase
	done   map[Phase]int64
	total  map[Phase]int64
}

func newProgressLog(t *testing.T) *progressLog {
	return &progressLog{t: t, done: map[Phase]int64{}, total: map[Phase]int64{}}
}

func (l *progressLog) report(p Phase, done, total int64) {
	if len(l.phases) == 0 || l.phases[len(l.phases)-1] != p {
		l.phases = append(l.phases, p)
		l.total[p] = total
	}
	if done < l.done[p] {
		l.t.Fatalf("phase %d went backwards: %d after %d", p, done, l.done[p])
	}
	if total != l.total[p] {
		l.t.Fatalf("phase %d total changed from %d to %d", p, l.total[p], total)
	}
	l.done[p] = done
}

func TestRestoreReportsProgress(t *testing.T) {
	h := newHarness(t)
	names, _ := chainScenario(t, h)
	for _, tc := range []struct {
		name string
		want []Phase
	}{
		{names[0], []Phase{PhaseChecking, PhaseWriting}}, // full
		{names[2], []Phase{PhaseChecking, PhaseWriting}}, // incremental: newer extents merged as each block is written
	} {
		log := newProgressLog(t)
		out := filepath.Join(h.dir, "progress-"+tc.name+".raw")
		os.Remove(out)
		if _, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: tc.name, Output: out, Progress: log.report}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(log.phases, tc.want) {
			t.Fatalf("%s: phases %v, want %v", tc.name, log.phases, tc.want)
		}
		m, err := h.repo.GetManifest(h.ctx, "win11", tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if log.total[PhaseWriting] != m.DiskSize || log.done[PhaseWriting] != m.DiskSize {
			t.Fatalf("%s: disk phase ended at %d of %d, disk is %d", tc.name, log.done[PhaseWriting], log.total[PhaseWriting], m.DiskSize)
		}
		if len(m.Overlay) == 0 && tc.name == names[2] {
			t.Fatalf("%s: the scenario's incremental has no newer extents", tc.name)
		}
	}
}

// Without a Progress func, restores work as before.
func TestRestoreWithoutProgress(t *testing.T) {
	h := newHarness(t)
	names, disks := chainScenario(t, h)
	restoreFrom(t, h, h.repo, "win11", names[2], disks[2])
}

// A whole-VM restore labels each disk's progress with its target.
func TestRestoreVMReportsEachDisk(t *testing.T) {
	v := newVMHarness(t)
	vda := filepath.Join(v.dir, "vda.img")
	randomDisk(t, vda, 192<<10, 4)
	v.virsh.xml = domainXML(fileDisk("vda", vda), v.nvram)
	st, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	_, err = RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: st.Record, Dir: filepath.Join(v.dir, "restored"),
		Progress: func(target string, p Phase, done, total int64) {
			if !slices.Contains(targets, target) {
				targets = append(targets, target)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(targets, []string{"vda"}) {
		t.Fatalf("progress reported for %v, want [vda]", targets)
	}
}
