// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/store"
)

// VMRecordKey is where a whole-VM backup record lives.
func VMRecordKey(vm, name string) string { return "vms/" + vm + "/" + name + ".vm" }

// PutVMRecord seals and stores a VM record. Like a manifest, it goes last.
func (r *Repo) PutVMRecord(ctx context.Context, rec *manifest.VMRecord, opts store.PutOptions) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	key := VMRecordKey(rec.VM, rec.Name)
	sealed, err := r.sealer.Seal([]byte(key), data)
	if err != nil {
		return err
	}
	return store.PutWith(ctx, r.Backend, key, sealed, opts)
}

// GetVMRecord reads and checks a VM record.
func (r *Repo) GetVMRecord(ctx context.Context, vm, name string) (*manifest.VMRecord, error) {
	key := VMRecordKey(vm, name)
	sealed, err := r.Backend.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("VM record %s/%s: %w", vm, name, err)
	}
	data, err := r.sealer.Open([]byte(key), sealed)
	if err != nil {
		return nil, fmt.Errorf("VM record %s/%s: %w", vm, name, err)
	}
	var rec manifest.VMRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("VM record %s/%s: %w", vm, name, err)
	}
	if rec.VM != vm || rec.Name != name {
		return nil, fmt.Errorf("VM record %s/%s names itself %s/%s", vm, name, rec.VM, rec.Name)
	}
	return &rec, rec.Validate()
}

// ListVMRecords lists a VM's records, oldest first.
func (r *Repo) ListVMRecords(ctx context.Context, vm string) ([]string, error) {
	keys, err := r.Backend.List(ctx, "vms/"+vm+"/")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, k := range keys {
		if n, ok := strings.CutSuffix(strings.TrimPrefix(k, "vms/"+vm+"/"), ".vm"); ok && !strings.Contains(n, "/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ListRecordVMs lists the VMs that have whole-VM records.
func (r *Repo) ListRecordVMs(ctx context.Context) ([]string, error) {
	keys, err := r.Backend.List(ctx, "vms/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var vms []string
	for _, k := range keys {
		parts := strings.Split(strings.TrimPrefix(k, "vms/"), "/")
		if len(parts) == 2 && !seen[parts[0]] {
			seen[parts[0]] = true
			vms = append(vms, parts[0])
		}
	}
	sort.Strings(vms)
	return vms, nil
}
