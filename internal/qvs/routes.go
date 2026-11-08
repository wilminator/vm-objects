// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package qvs

import "fmt"

// Every Virtualization Station route vmobj uses, in one place. Read from
// Virtualization Station 4.2.0's route and view definitions; the ones
// marked "inferred" follow its routes and Django REST Framework's
// conventions and are yet to be confirmed against a live system (vmobj
// qvs-check does that).

const (
	// QTS's own login: it returns a session id (SID).
	routeQTSLogin  = "/cgi-bin/authLogin.cgi"
	routeQTSLogout = "/cgi-bin/authLogout.cgi"
	// Virtualization Station's login takes the QTS SID (its
	// QTSSIDBackend) and sets a session cookie and a CSRF cookie. Inferred.
	routeLogin  = "/qvs/auth/login"
	routeCheck  = "/qvs/auth/check"
	routeLogout = "/qvs/auth/logout"

	routeVMs   = "/qvs/vms/"
	routeTasks = "/qvs/tasks/"
)

func routeVM(id string) string          { return fmt.Sprintf("/qvs/vms/%s/", id) }
func routeVMAction(id, a string) string { return fmt.Sprintf("/qvs/vms/%s/%s/", id, a) } // start, shutdown, forceshutdown
func routeSnapshots(vm string) string   { return fmt.Sprintf("/qvs/vms/%s/snapshots/", vm) }
func routeSnapshot(vm, id string) string {
	return fmt.Sprintf("/qvs/vms/%s/snapshots/%s/", vm, id)
}
func routeSnapshotRevert(vm, id string) string {
	return fmt.Sprintf("/qvs/vms/%s/snapshots/%s/revert/", vm, id)
}
func routeTask(id string) string { return fmt.Sprintf("/qvs/tasks/%s/", id) }

// The field names Virtualization Station uses, as far as known. Replies
// are read leniently (the first of several names that is present), since
// these are yet to be confirmed.
var (
	fieldID      = []string{"id", "pk"}
	fieldUUID    = []string{"uuid"}
	fieldName    = []string{"name"}
	fieldState   = []string{"status", "state", "power_state"}
	fieldTask    = []string{"task_id", "task", "id"}
	fieldCreated = []string{"creationTime", "creation_time", "created", "create_time"}
	fieldDesc    = []string{"description"}
	fieldLocked  = []string{"lockSnapshot", "lock_snapshot", "lockStatus"}
	fieldDisplay = []string{"displayname", "display_name", "name"}
	fieldResults = []string{"results", "data", "vms", "snapshots"}
	fieldTaskEnd = []string{"status", "state", "result"}
)
