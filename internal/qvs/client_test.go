// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package qvs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVS speaks Virtualization Station's protocol as confirmed on QTS:
// QTS login gives a SID, VS authenticates each request from the NAS_SID
// cookie, routes have no trailing slash, replies are {"status","data"}.
type fakeVS struct {
	mu       sync.Mutex
	password string
	snaps    []map[string]any
	failTask bool
	paths    []string // every request path, to prove nothing strays
	created  map[string]any
	polls    int
	unlocked bool
	// forceLock locks every new snapshot whatever was asked, as the
	// leftover from the first run is; forgetTasks skips the finished
	// summary and answers 3103 straight away.
	forceLock   bool
	forgetTasks bool
	newVM       []byte // the last create body
	made        bool
	// expireNext refuses the next authenticated request as a session that
	// has run out would; denyAll refuses every one. logins counts QTS logins.
	expireNext, denyAll bool
	logins              int
}

func (f *fakeVS) handler() http.Handler {
	ok := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": data})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/cgi-bin/authLogin.cgi":
			form, _ := url.ParseQuery(string(body))
			pwd, _ := base64.StdEncoding.DecodeString(form.Get("pwd"))
			if form.Get("user") != "vmobj" || string(pwd) != f.password {
				fmt.Fprint(w, `<QDocRoot><authPassed><![CDATA[0]]></authPassed></QDocRoot>`)
				return
			}
			f.logins++
			fmt.Fprint(w, `<QDocRoot><authPassed><![CDATA[1]]></authPassed><authSid><![CDATA[sid123]]></authSid></QDocRoot>`)
			return
		case "/cgi-bin/authLogout.cgi":
			return
		}
		if f.denyAll || (f.expireNext && r.URL.Path != "/qvs/auth/check") {
			f.expireNext = false
			json.NewEncoder(w).Encode(map[string]any{"status": 5, "detail": "Authentication credentials were not provided."})
			return
		}
		if ck, err := r.Cookie("NAS_SID"); err != nil || ck.Value != "sid123" {
			json.NewEncoder(w).Encode(map[string]any{"status": 5, "detail": "Authentication credentials were not provided."})
			return
		}
		switch p := r.URL.Path; {
		case p == "/qvs/auth/check", p == "/qvs/auth/logout":
			ok(w, nil)
		case p == "/qvs/vms" && r.Method == http.MethodGet:
			vms := []map[string]any{
				{"id": 20, "name": "WinGames", "uuid": "7240004c", "power_state": "stop"},
				{"id": 21, "name": "WinGames-xhost", "uuid": "42497a00", "power_state": "running",
					"graphics": []map[string]any{{"enable_password": "hunter2"}}},
			}
			if f.made {
				vms = append(vms, map[string]any{"id": 22, "name": "WinGames-restored", "uuid": "aaaa", "power_state": "stop"})
			}
			ok(w, vms)
		case p == "/qvs/v2/vms" && r.Method == http.MethodPost:
			var got map[string]any
			json.Unmarshal(body, &got)
			missing := map[string][]string{}
			for _, k := range []string{"auto_start", "auto_start_delay", "ballooning", "ballooning_rsvd", "bios", "cores", "cpu_model",
				"hot_plug_cpu", "keymap", "memory", "memory_sharing", "name", "usb", "video_type", "os_type", "is_agent_enabled", "sound"} {
				if _, ok := got[k]; !ok {
					missing[k] = []string{"This field is required."}
				}
			}
			if len(missing) > 0 {
				json.NewEncoder(w).Encode(map[string]any{"status": 1, "detail": missing})
				return
			}
			f.newVM, f.made = body, true
			json.NewEncoder(w).Encode(map[string]any{"status": 8, "data": map[string]any{"id": 3, "state": "waiting"}})
		case p == "/qvs/vms/21" && r.Method == http.MethodGet:
			ok(w, map[string]any{"id": 21, "power_state": "running"})
		case p == "/qvs/vms/21/snapshots" && r.Method == http.MethodGet:
			ok(w, f.snaps)
		case p == "/qvs/vms/21/snapshots" && r.Method == http.MethodPost:
			// As VS 4.2 does: it reads only these keys, locks unless
			// told "unlock", and queues the work as a task.
			json.Unmarshal(body, &f.created)
			lock, _ := f.created["lockSnapshot"].(string)
			if lock != "unlock" || f.forceLock {
				lock = "lock"
			}
			by, _ := f.created["createdBy"].(string)
			f.unlocked = lock == "unlock"
			f.snaps = append(f.snaps, map[string]any{"id": 1794103287, "vm_id": 21, "name": f.created["name"],
				"description": f.created["description"], "creation_time": "2026-11-08T02:01:27",
				"lock_snapshot": lock, "created_by": by})
			json.NewEncoder(w).Encode(map[string]any{"status": 8, "data": map[string]any{
				"id": 3, "user": "qts_vmobj", "state": "waiting", "type": "create_snapshot", "failed": false}})
		case p == "/qvs/tasks/3":
			f.polls++
			switch {
			case f.failTask:
				ok(w, map[string]any{"finished": true, "failed": true, "success": false, "error_message": "Task failed: unknown error"})
			case f.polls < 2:
				ok(w, map[string]any{"id": 3, "state": "waiting", "failed": false})
			case f.polls < 3:
				ok(w, map[string]any{"id": 3, "state": "running", "failed": false})
			case f.polls < 4 && !f.forgetTasks:
				ok(w, map[string]any{"finished": true, "failed": false, "success": true})
			default:
				json.NewEncoder(w).Encode(map[string]any{"status": 3103}) // forgotten
			}
		case p == "/qvs/vms/21/snapshots/1794103287/lock" && r.Method == http.MethodPost:
			var in map[string]any
			json.Unmarshal(body, &in)
			if in["lockStatus"] == "unlock" {
				f.unlocked = true
				for _, sn := range f.snaps {
					sn["lock_snapshot"] = "unlock"
				}
			}
			ok(w, nil)
		case p == "/qvs/vms/21/snapshots/1794103287" && r.Method == http.MethodDelete:
			if !f.unlocked {
				json.NewEncoder(w).Encode(map[string]any{"status": 2, "detail": "Snapshot is locked."})
				return
			}
			f.snaps = nil
			json.NewEncoder(w).Encode(map[string]any{"status": 8, "data": map[string]any{"id": 3, "state": "waiting"}})
		default:
			w.WriteHeader(http.StatusNotFound) // a trailing slash lands here, as on VS
		}
	})
}

func login(t *testing.T, f *fakeVS, password string) (*Client, error) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(&Config{URL: srv.URL, Username: "vmobj", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	c.PollEvery = time.Millisecond
	c.TaskTimeout = 2 * time.Second
	return c, c.Login(context.Background())
}

func TestLoginAndSnapshotLifecycle(t *testing.T) {
	f := &fakeVS{password: "p@ss w0rd+/=", forceLock: true}
	c, err := login(t, f, "p@ss w0rd+/=")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	vm, err := c.FindVM(ctx, "42497A00", "")
	if err != nil || vm.ID != 21 || vm.Name != "WinGames-xhost" {
		t.Fatalf("%+v %v", vm, err)
	}
	if st, _ := c.State(ctx, 21); st != "running" {
		t.Fatalf("state %q", st)
	}
	s, err := c.CreateSnapshot(ctx, 21, "vmobj-20261108T190000Z", "for a backup")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 1794103287 || s.Name != "vmobj-20261108T190000Z" || s.Created.Unix() != 1794103287 || !s.Locked {
		t.Fatalf("%+v", s)
	}
	if f.polls < 3 {
		t.Fatalf("didn't wait for the queued task (%d polls)", f.polls)
	}
	if f.created["lockSnapshot"] != "unlock" || f.created["createdBy"] != "user" || f.created["description"] != "for a backup" {
		t.Fatalf("created with %v", f.created)
	}
	f.polls = 0
	if err := c.DeleteSnapshot(ctx, 21, s.ID); err != nil {
		t.Fatal(err)
	}
	if !f.unlocked {
		t.Fatal("deleted a locked snapshot without unlocking it")
	}
	c.Logout(ctx)
	for _, p := range f.paths {
		if (strings.HasSuffix(p, "/") && p != "/") || p == "/qvs/auth/login" {
			t.Fatalf("requested %s: VS routes have no trailing slash, and its auth/login is QNE-only", p)
		}
	}
}

func TestLoginRefusals(t *testing.T) {
	f := &fakeVS{password: "right"}
	_, err := login(t, f, "wrong")
	if !errors.Is(err, ErrNotLoggedIn) || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("got %v", err)
	}
}

func TestFailedTask(t *testing.T) {
	f := &fakeVS{password: "p", failTask: true}
	c, err := login(t, f, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSnapshot(context.Background(), 21, "x", ""); !errors.Is(err, ErrTaskFailed) {
		t.Fatalf("got %v", err)
	}
}

// Ids are digits only, so nothing can turn a path into another route.
func TestParseIDRefusesInjection(t *testing.T) {
	for _, bad := range []string{"", "../1", "1/../../auth/logout", "%2f", "1?x=1", "1#", "-1", " 1", "1 ", "0x10", "١"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if id, err := ParseID("1794103287"); err != nil || routeSnapshot(21, id) != "/qvs/vms/21/snapshots/1794103287" {
		t.Fatalf("%v %v", id, err)
	}
}

func TestActionsAreAllowlisted(t *testing.T) {
	f := &fakeVS{password: "p"}
	c, err := login(t, f, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Act(context.Background(), 21, Action("../auth/logout")); err == nil {
		t.Fatal("an action outside the allowlist was sent")
	}
	for _, p := range f.paths {
		if strings.Contains(p, "..") {
			t.Fatalf("requested %s", p)
		}
	}
}

// A redirect is never followed, so the login form (with the password) is
// never resent to wherever it points.
func TestRedirectsAreRefused(t *testing.T) {
	var hits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := New(&Config{URL: srv.URL, Username: "vmobj", Password: "secret"})
	if err := c.Login(context.Background()); err == nil {
		t.Fatal("logged in through a redirect")
	}
	if hits != 0 {
		t.Fatal("the redirect was followed: the login form went to another server")
	}
}

func TestURLChecks(t *testing.T) {
	for _, good := range []string{"http://127.0.0.1:8080", "http://localhost", "http://[::1]:8080", "https://nas.example:443", "https://192.168.0.200"} {
		if _, err := checkURL(good); err != nil {
			t.Errorf("%s refused: %v", good, err)
		}
	}
	for _, bad := range []string{"http://192.168.0.200:8080", "ftp://127.0.0.1", "https://u:p@nas", "https://nas/qvs", "https://nas?x=1", "127.0.0.1:8080", ""} {
		if _, err := checkURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Server text is made safe to print: no escape sequences or line breaks,
// and nothing that looks like a secret.
func TestErrorTextIsCleaned(t *testing.T) {
	in := "bad\x1b[2J\x1b]0;owned\x07 input\nFAKE LOG LINE {\"enable_password\":\"hunter2\",\"csrftoken\":\"abc\"} sid=XYZ"
	out := clean(in)
	for _, bad := range []string{"\x1b", "\n", "\x07", "hunter2", "\"abc\"", "XYZ"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q kept %q", out, bad)
		}
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("%q", out)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "qvs.conf")
	os.WriteFile(p, []byte("# vmobj\n[other]\nurl = x\n[qvs]\nurl = http://127.0.0.1:8080/\nusername = vmobj\npassword = s3cret = yes\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil || c.URL != "http://127.0.0.1:8080" || c.Username != "vmobj" || c.Password != "s3cret = yes" {
		t.Fatalf("%+v %v", c, err)
	}
	os.Chmod(p, 0o644)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a world-readable password file was accepted: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "remote"), []byte("[qvs]\nurl = http://192.168.0.200:8080\nusername = u\npassword = p\n"), 0o600)
	if _, err := LoadConfig(filepath.Join(dir, "remote")); err == nil {
		t.Fatal("plain http to another machine was accepted")
	}
}

func TestSharedPath(t *testing.T) {
	for in, want := range map[string]string{
		"/share/VMs/m9/WinGames-vda.qcow2":                "shared://VMs/m9/WinGames-vda.qcow2",
		"/share/CACHEDEV1_DATA/VMs/m9/WinGames-vda.qcow2": "shared://VMs/m9/WinGames-vda.qcow2",
	} {
		if got, err := SharedPath(in); err != nil || got != want {
			t.Errorf("%s -> %s, %v", in, got, err)
		}
	}
	for _, bad := range []string{"/share/VMs", "/share/VMs/../etc/x", "/etc/passwd", "share/VMs/x", "/share/VMs/a$(id)", "/share/VMs/a\nb", "/share/.qpkg/x", "/share/VMs//x"} {
		if _, err := SharedPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The create payload uses existing images by shared:// path, never
// sends a display password, and the VM is waited for.
func TestCreateVM(t *testing.T) {
	f := &fakeVS{password: "p"}
	c, err := login(t, f, "p")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := CreateSpec{Name: "WinGames-restored", OSType: "win11", UEFI: true, Machine: "q35", Cores: 4, Memory: 16 << 30,
		Disks: []CreateDisk{{Path: "/share/VMs/m9/WinGames-vda.qcow2", Bus: "virtio"}}}
	vm, err := c.CreateVM(ctx, spec)
	if err != nil || vm.ID != 22 {
		t.Fatalf("%+v %v", vm, err)
	}
	var sent map[string]any
	json.Unmarshal(f.newVM, &sent)
	disk := sent["disks"].([]any)[0].(map[string]any)
	g := sent["graphics"].([]any)[0].(map[string]any)
	if sent["bios"] != "ovmf" || disk["creating_image"] != false || disk["path"] != "shared://VMs/m9/WinGames-vda.qcow2" ||
		g["enable_password"] != false || g["localhost_only"] != true || strings.Contains(string(f.newVM), `"password"`) {
		t.Fatalf("sent %s", f.newVM)
	}
	if _, err := c.CreateVM(ctx, spec); err == nil {
		t.Fatal("created a second VM with the same name")
	}
	for _, bad := range []CreateSpec{
		{Name: "-x", OSType: "win11", Cores: 1, Memory: 1 << 30, Disks: spec.Disks},
		{Name: "ok", OSType: "win11; rm", Cores: 1, Memory: 1 << 30, Disks: spec.Disks},
		{Name: "ok", OSType: "win11", Cores: 1, Memory: 1 << 30, Disks: []CreateDisk{{Path: "/etc/shadow"}}},
		{Name: "ok", OSType: "win11", Cores: 1, Memory: 1 << 30, Disks: spec.Disks, Bridges: []string{"br0&x=1"}},
	} {
		if _, err := c.CreateVM(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// A snapshot asked for unlocked comes out unlocked, and a task VS has
// already forgotten (status 3103) counts as done: the snapshot list is
// what confirms it.
func TestUnlockedSnapshotAndForgottenTask(t *testing.T) {
	f := &fakeVS{password: "p", forgetTasks: true}
	c, err := login(t, f, "p")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := c.CreateSnapshot(ctx, 21, "vmobj-20261109T010000Z", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Locked {
		t.Fatalf("asked for unlocked, got %+v", s)
	}
	if err := c.DeleteSnapshot(ctx, 21, s.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.snaps) != 0 {
		t.Fatalf("left %v", f.snaps)
	}
}

// A session that ran out (a restore that took hours) is replaced with one
// new login and the request repeated; a second refusal is not retried.
func TestSessionExpiryLogsInAgain(t *testing.T) {
	f := &fakeVS{password: "p"}
	c, err := login(t, f, "p")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.mu.Lock()
	f.expireNext = true
	f.mu.Unlock()
	vm, err := c.FindVM(ctx, "42497A00", "")
	if err != nil || vm.ID != 21 {
		t.Fatalf("%+v %v", vm, err)
	}
	if f.logins != 2 {
		t.Fatalf("%d logins, want 2", f.logins)
	}
	f.mu.Lock()
	f.denyAll = true
	f.mu.Unlock()
	if _, err := c.FindVM(ctx, "42497A00", ""); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("got %v", err)
	}
	if f.logins != 3 {
		t.Fatalf("%d logins: it should log in once more, not loop", f.logins)
	}
}

// The create payload carries every field VS requires, with VS's own values,
// "sound" as an explicit null, no machine type (VS picks one its QEMU
// knows) and nothing that starts the VM or sets a password.
func TestCreateVMPayloadHasTheRequiredFields(t *testing.T) {
	f := &fakeVS{password: "p"}
	c, err := login(t, f, "p")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateVM(context.Background(), CreateSpec{Name: "WinGames-restored", OSType: "win11", UEFI: true,
		Cores: 4, Memory: 8 << 30,
		Disks: []CreateDisk{{Path: "/share/VMs/x/WinGames-m9-vda.qcow2", Bus: "virtio"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(f.newVM, &got); err != nil {
		t.Fatalf("the fake VS refused the payload (%v): %s", err, f.newVM)
	}
	if v, ok := got["sound"]; !ok || v != nil {
		t.Errorf("sound must be an explicit null: %v", got["sound"])
	}
	if got["auto_start"] != "off" || got["keymap"] != "en-us" || got["cpu_model"] != "host" || got["bios"] != "ovmf" {
		t.Errorf("payload %v", got)
	}
	if _, ok := got["machine"]; ok {
		t.Errorf("machine %v was sent; VS should pick its own", got["machine"])
	}
	if strings.Contains(string(f.newVM), "password\":\"") {
		t.Errorf("a password was sent: %s", f.newVM)
	}
}

// Field errors come back as an object; every field name is in the error.
func TestCreateRefusalNamesEveryField(t *testing.T) {
	e := envelope{Detail: json.RawMessage(`{"auto_start":["This field is required."],"keymap":["This field is required."],"zzz_last":["x"]}`)}
	if got := clean(e.text()); !strings.Contains(got, "auto_start") || !strings.Contains(got, "zzz_last") {
		t.Fatalf("%s", got)
	}
}
