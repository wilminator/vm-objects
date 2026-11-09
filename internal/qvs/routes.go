// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package qvs

import (
	"fmt"
	"strconv"
)

// Every Virtualization Station route vmobj uses, in one place: a closed
// set. Confirmed on Virtualization Station 4.2.0.281 (QTS 5.2) unless
// marked "inferred" (from its route definitions; confirmed by the first
// write test). Routes have no trailing slash: with one, VS answers 404.
//
// Ids are typed numbers, formatted with %d, so nothing from a command
// line or a reply can turn a path into another route ("../", "?", "#").

const (
	routeQTSLogin  = "/cgi-bin/authLogin.cgi"  // confirmed
	routeQTSLogout = "/cgi-bin/authLogout.cgi" // confirmed; QTS wants ?sid=
	routeCheck     = "/qvs/auth/check"         // confirmed: data status 0 = logged in
	routeLogout    = "/qvs/auth/logout"        // confirmed
	routeVMs       = "/qvs/vms"                // confirmed
)

// sidCookie is the cookie Virtualization Station's QTS middleware reads
// the QTS session id from (confirmed over http on loopback).
const sidCookie = "NAS_SID"

// ID is a Virtualization Station id: a VM, a snapshot (its epoch), a
// task. All are numbers.
type ID uint64

// ParseID reads an id, accepting decimal digits only.
func ParseID(s string) (ID, error) {
	if s == "" || len(s) > 20 {
		return 0, fmt.Errorf("qvs: %q is not an id", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("qvs: %q is not an id (digits only)", s)
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("qvs: %q is not an id", s)
	}
	return ID(n), nil
}

func (i ID) String() string { return strconv.FormatUint(uint64(i), 10) }

// Action is a VM power action: only these are ever sent.
type Action string

const (
	ActionStart         Action = "start"         // inferred
	ActionShutdown      Action = "shutdown"      // inferred
	ActionForceShutdown Action = "forceshutdown" // inferred
)

func (a Action) valid() bool {
	switch a {
	case ActionStart, ActionShutdown, ActionForceShutdown:
		return true
	}
	return false
}

func routeVM(id ID) string                 { return fmt.Sprintf("/qvs/vms/%d", id) }                    // confirmed
func routeVMAction(id ID, a Action) string { return fmt.Sprintf("/qvs/vms/%d/%s", id, a) }              // inferred; a is allowlisted
func routeSnapshots(vm ID) string          { return fmt.Sprintf("/qvs/vms/%d/snapshots", vm) }          // GET confirmed; POST inferred
func routeSnapshot(vm, snap ID) string     { return fmt.Sprintf("/qvs/vms/%d/snapshots/%d", vm, snap) } // DELETE inferred
func routeSnapshotRevert(vm, snap ID) string {
	return fmt.Sprintf("/qvs/vms/%d/snapshots/%d/revert", vm, snap) // inferred
}
func routeTask(id ID) string { return fmt.Sprintf("/qvs/tasks/%d", id) }

// Envelope and field names, as confirmed: every reply is
// {"status": N, "data": ...}, or {"status": N, "detail": "..."} on error.
const (
	statusOK               = 0
	statusNotAuthenticated = 5
	statusQueued           = 8    // data is a task; wait for it
	statusTaskNotFound     = 3103 // the task finished and VS has forgotten it
)

func routeSnapshotLock(vm, snap ID) string {
	return fmt.Sprintf("/qvs/vms/%d/snapshots/%d/lock", vm, snap)
}

// snapshotCreate is the body of POST routeSnapshots: the only keys
// SnapshotViewSet.create reads (VS 4.2). It has no disk-only option: VS
// saves a running VM's memory with the snapshot (a .state file the size
// of its RAM), which deleting the snapshot removes again.
type snapshotCreate struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	LockSnapshot string `json:"lockSnapshot"` // "lock" (VS's default) or "unlock"
	CreatedBy    string `json:"createdBy"`    // "user", as the UI does
}

// snapshotUnlock is the body of POST routeSnapshotLock, which replies at
// once (no task). DELETE refuses a locked snapshot.
type snapshotUnlock struct {
	LockStatus string `json:"lockStatus"`
}

// task is a Virtualization Station background task (confirmed shape:
// id, state "waiting"..., failed, progress).
//
// While it runs, GET routeTask gives the task with state "waiting" or
// "running". Once it ends VS keeps a short-lived summary (finished,
// failed, success, error_message), and after that answers
// statusTaskNotFound.
type task struct {
	ID       ID     `json:"id"`
	State    string `json:"state"`
	Failed   bool   `json:"failed"`
	Finished bool   `json:"finished"`
	Success  bool   `json:"success"`
	Error    string `json:"error_message"`
}
