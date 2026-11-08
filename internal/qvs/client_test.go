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
			fmt.Fprint(w, `<QDocRoot><authPassed><![CDATA[1]]></authPassed><authSid><![CDATA[sid123]]></authSid></QDocRoot>`)
			return
		case "/cgi-bin/authLogout.cgi":
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
			ok(w, []map[string]any{
				{"id": 20, "name": "WinGames", "uuid": "7240004c", "power_state": "stop"},
				{"id": 21, "name": "WinGames-xhost", "uuid": "42497a00", "power_state": "running",
					"graphics": []map[string]any{{"enable_password": "hunter2"}}},
			})
		case p == "/qvs/vms/21" && r.Method == http.MethodGet:
			ok(w, map[string]any{"id": 21, "power_state": "running"})
		case p == "/qvs/vms/21/snapshots" && r.Method == http.MethodGet:
			ok(w, f.snaps)
		case p == "/qvs/vms/21/snapshots" && r.Method == http.MethodPost:
			json.Unmarshal(body, &f.created)
			f.snaps = append(f.snaps, map[string]any{"id": 1794103287, "vm_id": 21, "name": f.created["name"],
				"description": f.created["description"], "creation_time": "2026-11-08T02:01:27",
				"lock_snapshot": f.created["lock_snapshot"], "created_by": "user"})
			ok(w, map[string]any{"task_id": 9})
		case p == "/qvs/tasks/9":
			if f.failTask {
				ok(w, map[string]any{"status": "failed"})
			} else {
				w.WriteHeader(http.StatusNotFound) // finished tasks are cleaned up
				json.NewEncoder(w).Encode(map[string]any{"status": 4, "detail": "Not found."})
			}
		case p == "/qvs/vms/21/snapshots/1794103287" && r.Method == http.MethodDelete:
			f.snaps = nil
			ok(w, nil)
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
	f := &fakeVS{password: "p@ss w0rd+/="}
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
	if s.ID != 1794103287 || s.Name != "vmobj-20261108T190000Z" || s.Created.Unix() != 1794103287 || s.Locked {
		t.Fatalf("%+v", s)
	}
	if f.created["lock_snapshot"] != "unlock" || f.created["description"] != "for a backup" {
		t.Fatalf("created with %v", f.created)
	}
	if err := c.DeleteSnapshot(ctx, 21, s.ID); err != nil {
		t.Fatal(err)
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
