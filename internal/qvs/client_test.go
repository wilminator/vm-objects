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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVS is a stand-in for Virtualization Station: QTS login, its own
// session and CSRF cookies, VMs, snapshots made by a background task.
type fakeVS struct {
	mu        sync.Mutex
	password  string
	snaps     []map[string]any
	taskPolls int
	failTask  bool
	csrfFails int
}

func (f *fakeVS) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/authLogin.cgi", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		pwd, _ := base64.StdEncoding.DecodeString(r.Form.Get("pwd"))
		if r.Form.Get("user") != "vmobj" || string(pwd) != f.password {
			fmt.Fprint(w, `<QDocRoot><authPassed><![CDATA[0]]></authPassed></QDocRoot>`)
			return
		}
		fmt.Fprint(w, `<QDocRoot><authPassed><![CDATA[1]]></authPassed><authSid><![CDATA[sid123]]></authSid></QDocRoot>`)
	})
	mux.HandleFunc("/qvs/auth/login", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("sid") != "sid123" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "s1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: "c1", Path: "/"})
	})
	mux.HandleFunc("/qvs/auth/logout", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/cgi-bin/authLogout.cgi", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/qvs/", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("sessionid"); err != nil || ck.Value != "s1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-CSRFToken") != "c1" {
			f.mu.Lock()
			f.csrfFails++
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/qvs/vms/" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 20, "name": "WinGames", "uuid": "7240004c", "status": "stopped"},
				{"id": 21, "name": "WinGames-xhost", "uuid": "42497a00", "status": "running"},
			})
		case p == "/qvs/vms/21/" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"id": 21, "status": "Running"})
		case p == "/qvs/vms/21/snapshots/" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"results": f.snaps})
		case p == "/qvs/vms/21/snapshots/" && r.Method == http.MethodPost:
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.snaps = append(f.snaps, map[string]any{"name": "1794103287", "displayname": in["name"],
				"description": in["description"], "creationTime": 1794103287, "lockSnapshot": in["lockSnapshot"]})
			json.NewEncoder(w).Encode(map[string]any{"task_id": 9})
		case p == "/qvs/tasks/9/":
			f.taskPolls++
			switch {
			case f.failTask:
				json.NewEncoder(w).Encode(map[string]any{"status": "failed"})
			case f.taskPolls < 2:
				json.NewEncoder(w).Encode(map[string]any{"status": "running"})
			default:
				json.NewEncoder(w).Encode(map[string]any{"status": "done"})
			}
		case p == "/qvs/vms/21/snapshots/1794103287/" && r.Method == http.MethodDelete:
			f.snaps = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return mux
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
	return c, c.Login(context.Background())
}

func TestLoginAndSnapshotLifecycle(t *testing.T) {
	f := &fakeVS{password: "p@ss w0rd"}
	c, err := login(t, f, "p@ss w0rd")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	vm, err := c.FindVM(ctx, "42497A00", "")
	if err != nil || vm.ID != "21" || vm.Name != "WinGames-xhost" {
		t.Fatalf("%+v %v", vm, err)
	}
	if st, _ := c.State(ctx, "21"); st != "running" {
		t.Fatalf("state %q", st)
	}
	s, err := c.CreateSnapshot(ctx, "21", "vmobj-20261108T190000Z", "for a backup")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "1794103287" || s.Name != "vmobj-20261108T190000Z" || s.Created.Unix() != 1794103287 || s.Locked {
		t.Fatalf("%+v", s)
	}
	if f.taskPolls < 2 {
		t.Fatal("didn't wait for the task")
	}
	if err := c.DeleteSnapshot(ctx, "21", s.ID); err != nil {
		t.Fatal(err)
	}
	if f.csrfFails != 0 {
		t.Fatalf("%d requests without the CSRF token", f.csrfFails)
	}
	c.Logout(ctx)
}

func TestLoginRefusals(t *testing.T) {
	f := &fakeVS{password: "right"}
	if _, err := login(t, f, "wrong"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(fmt.Sprint(login(t, f, "wrong")), "wrong") {
		t.Fatal("the password appears in the error")
	}
}

func TestFailedTask(t *testing.T) {
	f := &fakeVS{password: "p", failTask: true}
	c, err := login(t, f, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSnapshot(context.Background(), "21", "x", ""); !errors.Is(err, ErrTaskFailed) {
		t.Fatalf("got %v", err)
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
	os.WriteFile(filepath.Join(dir, "short"), []byte("[qvs]\nurl = x\n"), 0o600)
	if _, err := LoadConfig(filepath.Join(dir, "short")); err == nil {
		t.Fatal("a file without a password was accepted")
	}
}
