// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"errors"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
)

// newS3Harness returns a harness on the fake S3 and the fake itself.
func newS3Harness(t *testing.T) (*harness, *s3fake.Server) {
	t.Helper()
	srv := s3fake.New("bucket")
	b, err := store.NewS3(srv, "bucket", "repo")
	if err != nil {
		t.Fatal(err)
	}
	return newHarnessOn(t, b), srv
}

// classOf returns the storage class of every object under a prefix.
func classOf(srv *s3fake.Server, h *harness, prefix string) map[types.StorageClass]int {
	out := map[types.StorageClass]int{}
	keys, _ := h.repo.Backend.List(h.ctx, prefix)
	for _, k := range keys {
		out[srv.Object("bucket", "repo/"+k).Class]++
	}
	return out
}

// The disaster-recovery profile puts full-backup blocks in Deep Archive and
// incremental packs in Standard-IA, while everything needed to list, plan
// and index stays in Standard. Restoring the full then needs a restore
// request; restoring nothing archived does not.
func TestProfilePlacesDataByKind(t *testing.T) {
	h, srv := newS3Harness(t)
	prof, err := policy.Lookup("disaster-recovery")
	if err != nil {
		t.Fatal(err)
	}
	h.profile = &prof

	disk := make([]byte, 512<<10)
	rand.New(rand.NewSource(9)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full)
	full1 := append([]byte(nil), disk...)
	rand.New(rand.NewSource(10)).Read(disk[64<<10 : 72<<10])
	h.write(disk)
	inc := h.backup(manifest.Incremental)

	if got := classOf(srv, h, "blocks/"); got[types.StorageClassDeepArchive] == 0 || len(got) != 1 {
		t.Fatalf("block classes = %v, want all DEEP_ARCHIVE", got)
	}
	packs := map[types.StorageClass]int{}
	tocs := map[types.StorageClass]int{}
	keys, _ := h.repo.Backend.List(h.ctx, "packs/")
	for _, k := range keys {
		c := srv.Object("bucket", "repo/"+k).Class
		if strings.HasSuffix(k, ".toc") {
			tocs[c]++
		} else {
			packs[c]++
		}
	}
	if packs[types.StorageClassStandardIa] == 0 || len(packs) != 1 {
		t.Fatalf("pack classes = %v, want all STANDARD_IA", packs)
	}
	if tocs[""] == 0 || len(tocs) != 1 {
		t.Fatalf("TOC classes = %v, want Standard", tocs)
	}
	if got := classOf(srv, h, "manifests/"); got[""] != 2 || len(got) != 1 {
		t.Fatalf("manifest classes = %v, want Standard", got)
	}

	// The manifests record the policy they were made under.
	m, err := h.repo.GetManifest(h.ctx, "win11", inc.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if m.Profile != "disaster-recovery" || m.DataClass != "STANDARD_IA" ||
		!m.RetainUntil.Equal(m.Created.Add(30*policy.Day)) {
		t.Fatalf("incremental manifest policy: %q %q %v", m.Profile, m.DataClass, m.RetainUntil)
	}

	// Restoring needs the archived blocks back first. Nothing is written
	// while they are archived.
	out := filepath.Join(h.dir, "full.raw")
	_, err = Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: full.Manifest, Output: out})
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		t.Fatalf("restore from Deep Archive: %v, want NotReadyError", err)
	}
	rd := nr.Readiness
	if len(rd.Pending) == 0 || rd.Ready != 0 || len(rd.Classes) != 1 || rd.Classes[0] != policy.DeepArchive {
		t.Fatalf("readiness = %+v", rd)
	}
	if !strings.Contains(rd.Wait(policy.Bulk), "48h") {
		t.Fatalf("bulk wait = %s", rd.Wait(policy.Bulk))
	}

	// A deep verify can't read archived pieces, and says so, rather than
	// reporting them as damaged.
	vs, err := Verify(h.ctx, h.repo, "win11", full.Manifest, true)
	if err != nil || len(vs.Missing) != 0 || vs.Archived == 0 {
		t.Fatalf("deep verify of archived data: %+v, %v", vs, err)
	}

	// Deep Archive has no Expedited tier.
	if _, err := RequestRestores(h.ctx, h.repo, rd, 3, policy.Expedited); err == nil {
		t.Fatal("expedited restore from Deep Archive was accepted")
	}
	n, err := RequestRestores(h.ctx, h.repo, rd, 3, policy.Bulk)
	if err != nil || n != len(rd.Pending) {
		t.Fatalf("RequestRestores = %d, %v", n, err)
	}
	rd2, _ := CheckReady(h.ctx, h.repo, "win11", full.Manifest)
	if rd2.Restoring != n || len(rd2.Pending) != 0 {
		t.Fatalf("after requesting: %+v", rd2)
	}
	if _, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: full.Manifest, Output: out}); err == nil {
		t.Fatal("restore ran while restores were still in progress")
	}

	// When S3 finishes, the restore goes through.
	srv.CompleteRestores()
	h.restoreEquals(full.Manifest, full1)
}

// --class overrides the profile for one run, and a service that only knows
// Standard explains the problem instead of failing obscurely.
func TestClassOverrideAndUnsupportedClass(t *testing.T) {
	h, srv := newS3Harness(t)
	prof, _ := policy.Lookup("disaster-recovery")
	h.profile = &prof
	h.class = policy.GlacierIR
	disk := make([]byte, 128<<10)
	rand.New(rand.NewSource(2)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full)
	if got := classOf(srv, h, "blocks/"); got[types.StorageClassGlacierIr] == 0 || len(got) != 1 {
		t.Fatalf("classes with --class GLACIER_IR = %v", got)
	}
	// Glacier Instant Retrieval needs no restore request.
	h.restoreEquals(full.Manifest, disk)

	srv.Classes = []types.StorageClass{"", types.StorageClassStandard}
	rand.New(rand.NewSource(3)).Read(disk)
	h.write(disk)
	h.class = ""
	_, err := h.tryBackup(manifest.Full)
	if err == nil || !strings.Contains(err.Error(), "--profile standard") {
		t.Fatalf("backup to a Standard-only service: %v", err)
	}
}

func TestBuiltinProfilesValid(t *testing.T) {
	for _, name := range policy.Names() {
		p, err := policy.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		warn, err := p.Validate()
		if err != nil || len(warn) > 0 {
			t.Errorf("profile %s: %v %v", name, err, warn)
		}
	}
}
