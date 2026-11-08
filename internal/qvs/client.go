// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package qvs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Client is a logged-in session with Virtualization Station.
type Client struct {
	cfg  *Config
	http *http.Client
	sid  string
	// PollEvery and TaskTimeout govern waiting for Virtualization
	// Station's background tasks (a snapshot, a shutdown).
	PollEvery   time.Duration
	TaskTimeout time.Duration
}

// New returns a client; call Login before anything else.
func New(cfg *Config) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, http: &http.Client{Jar: jar, Timeout: 2 * time.Minute},
		PollEvery: 2 * time.Second, TaskTimeout: 30 * time.Minute}, nil
}

// ErrNotLoggedIn means a login was refused or the session ended.
var ErrNotLoggedIn = errors.New("qvs: not logged in")

type qtsLogin struct {
	AuthPassed string `xml:"authPassed"`
	AuthSid    string `xml:"authSid"`
	Need2SV    string `xml:"need_2sv"`
	ErrorValue string `xml:"errorValue"`
}

// Login logs in to QTS (which gives a session id), then to Virtualization
// Station with it (which sets its session and CSRF cookies). The password
// is never logged or put in a URL.
func (c *Client) Login(ctx context.Context) error {
	form := url.Values{
		"user":       {c.cfg.Username},
		"pwd":        {base64.StdEncoding.EncodeToString([]byte(c.cfg.Password))}, // QTS takes it base64-encoded
		"serviceKey": {"1"},
	}
	body, status, err := c.raw(ctx, http.MethodPost, routeQTSLogin, "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return fmt.Errorf("qts login: %w", err)
	}
	var r qtsLogin
	if err := xml.Unmarshal(body, &r); err != nil || status != http.StatusOK {
		return fmt.Errorf("qts login: unexpected reply (HTTP %d)", status)
	}
	switch {
	case strings.TrimSpace(r.Need2SV) == "1":
		return fmt.Errorf("%w: the account has 2-step verification on; vmobj's account needs it off", ErrNotLoggedIn)
	case strings.TrimSpace(r.AuthPassed) != "1" || strings.TrimSpace(r.AuthSid) == "":
		return fmt.Errorf("%w: QTS refused the username or password for %q", ErrNotLoggedIn, c.cfg.Username)
	}
	c.sid = strings.TrimSpace(r.AuthSid)
	vsForm := url.Values{"sid": {c.sid}}
	if _, status, err = c.raw(ctx, http.MethodPost, routeLogin, "application/x-www-form-urlencoded", []byte(vsForm.Encode())); err != nil {
		return fmt.Errorf("virtualization station login: %w", err)
	}
	if status >= 300 {
		return fmt.Errorf("%w: Virtualization Station refused the session (HTTP %d); does %q have Virtualization Station rights?", ErrNotLoggedIn, status, c.cfg.Username)
	}
	return nil
}

// Logout ends both sessions. Errors are ignored: it's cleanup.
func (c *Client) Logout(ctx context.Context) {
	c.raw(ctx, http.MethodPost, routeLogout, "application/json", nil)
	if c.sid != "" {
		c.raw(ctx, http.MethodGet, routeQTSLogout+"?sid="+url.QueryEscape(c.sid), "", nil)
		c.sid = ""
	}
}

func (c *Client) csrf() string {
	u, err := url.Parse(c.cfg.URL)
	if err != nil {
		return ""
	}
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == "csrftoken" {
			return ck.Value
		}
	}
	return ""
}

// raw sends one request and returns the body and status. Django wants
// the CSRF token in a header (and a Referer) on anything that changes
// something.
func (c *Client) raw(ctx context.Context, method, path, ctype string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", c.cfg.URL+"/qvs/")
	if method != http.MethodGet && method != http.MethodHead {
		if t := c.csrf(); t != "" {
			req.Header.Set("X-CSRFToken", t)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return b, resp.StatusCode, err
}

// APIError is a reply Virtualization Station refused, with the start of
// its body (which never holds the session or the password).
type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("qvs %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// call sends JSON and decodes a JSON reply into out (when non-nil).
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	b, status, err := c.raw(ctx, method, path, "application/json", body)
	if err != nil {
		return fmt.Errorf("qvs %s %s: %w", method, path, err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%w (qvs %s %s: HTTP %d)", ErrNotLoggedIn, method, path, status)
	}
	if status >= 300 {
		s := strings.TrimSpace(string(b))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		return &APIError{Method: method, Path: path, Status: status, Body: s}
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("qvs %s %s: reply isn't the JSON expected: %w", method, path, err)
	}
	return nil
}

// obj is a JSON object read leniently.
type obj map[string]any

func (o obj) str(names []string) string {
	for _, n := range names {
		switch v := o[n].(type) {
		case string:
			return v
		case float64:
			return fmt.Sprintf("%.0f", v)
		case bool:
			return fmt.Sprint(v)
		}
	}
	return ""
}

// list reads a reply that is a list, or an object holding one.
func list(raw json.RawMessage) ([]obj, error) {
	var arr []obj
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	for _, k := range fieldResults {
		if inner, ok := wrap[k]; ok {
			return list(inner)
		}
	}
	return nil, errors.New("no list in the reply")
}

// VM is Virtualization Station's view of a VM.
type VM struct {
	ID, UUID, Name, State string
}

// ListVMs lists every VM.
func (c *Client) ListVMs(ctx context.Context) ([]VM, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodGet, routeVMs, nil, &raw); err != nil {
		return nil, err
	}
	items, err := list(raw)
	if err != nil {
		return nil, fmt.Errorf("qvs: listing VMs: %w", err)
	}
	var vms []VM
	for _, o := range items {
		vms = append(vms, VM{ID: o.str(fieldID), UUID: o.str(fieldUUID), Name: o.str(fieldName), State: o.str(fieldState)})
	}
	return vms, nil
}

// FindVM finds a VM by UUID (preferred: names can repeat) or name.
func (c *Client) FindVM(ctx context.Context, uuid, name string) (*VM, error) {
	vms, err := c.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	for i := range vms {
		if uuid != "" && strings.EqualFold(vms[i].UUID, uuid) {
			return &vms[i], nil
		}
	}
	var found *VM
	for i := range vms {
		if uuid == "" && vms[i].Name == name {
			if found != nil {
				return nil, fmt.Errorf("qvs: two VMs are named %q; use its UUID", name)
			}
			found = &vms[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("qvs: no VM with UUID %q or name %q", uuid, name)
	}
	return found, nil
}

// State reads one VM's state, normalised to running, stopped or paused
// (or the raw value when it is none of those).
func (c *Client) State(ctx context.Context, id string) (string, error) {
	var o obj
	if err := c.call(ctx, http.MethodGet, routeVM(id), nil, &o); err != nil {
		return "", err
	}
	return NormalState(o.str(fieldState)), nil
}

// NormalState maps Virtualization Station's state words to running,
// stopped or paused.
func NormalState(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "run"):
		return "running"
	case strings.Contains(l, "pause"), strings.Contains(l, "suspend"):
		return "paused"
	case strings.Contains(l, "stop"), strings.Contains(l, "off"), strings.Contains(l, "shut"):
		return "stopped"
	}
	return s
}

// Action runs a VM action (start, shutdown, forceshutdown) and waits for
// its task when it returns one.
func (c *Client) Action(ctx context.Context, id, action string) error {
	var o obj
	if err := c.call(ctx, http.MethodPost, routeVMAction(id, action), map[string]any{}, &o); err != nil {
		return err
	}
	return c.waitIfTask(ctx, o)
}

// Snapshot is Virtualization Station's view of a snapshot.
type Snapshot struct {
	ID          string // the epoch name it is filed under
	Name        string // the name shown in its UI
	Description string
	Created     time.Time
	Locked      bool
}

// ListSnapshots lists a VM's snapshots.
func (c *Client) ListSnapshots(ctx context.Context, vmID string) ([]Snapshot, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodGet, routeSnapshots(vmID), nil, &raw); err != nil {
		return nil, err
	}
	items, err := list(raw)
	if err != nil {
		return nil, fmt.Errorf("qvs: listing snapshots: %w", err)
	}
	var out []Snapshot
	for _, o := range items {
		s := Snapshot{ID: o.str(fieldID), Name: o.str(fieldDisplay), Description: o.str(fieldDesc)}
		if s.ID == "" {
			s.ID = o.str(fieldName) // libvirt's name: the epoch
		}
		lock := strings.ToLower(o.str(fieldLocked))
		s.Locked = lock == "lock" || lock == "locked" || lock == "true"
		if t := o.str(fieldCreated); t != "" {
			if secs, err := parseEpoch(t); err == nil {
				s.Created = secs
			} else if tm, err := time.Parse(time.RFC3339, t); err == nil {
				s.Created = tm
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func parseEpoch(s string) (time.Time, error) {
	var secs int64
	if _, err := fmt.Sscanf(s, "%d", &secs); err != nil || fmt.Sprint(secs) != s {
		return time.Time{}, errors.New("not an epoch")
	}
	return time.Unix(secs, 0).UTC(), nil
}

// CreateSnapshot takes a snapshot as the UI does, waits for it, and
// returns it as listed afterwards.
func (c *Client) CreateSnapshot(ctx context.Context, vmID, name, description string) (*Snapshot, error) {
	var o obj
	in := map[string]any{"name": name, "description": description, "lockSnapshot": "unlock", "createdBy": "user"}
	if err := c.call(ctx, http.MethodPost, routeSnapshots(vmID), in, &o); err != nil {
		return nil, err
	}
	if err := c.waitIfTask(ctx, o); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	snaps, err := c.ListSnapshots(ctx, vmID)
	if err != nil {
		return nil, err
	}
	for i := range snaps {
		if snaps[i].Name == name {
			return &snaps[i], nil
		}
	}
	return nil, fmt.Errorf("qvs: snapshot %q isn't listed after taking it", name)
}

// DeleteSnapshot deletes a snapshot and waits for it.
func (c *Client) DeleteSnapshot(ctx context.Context, vmID, id string) error {
	var o obj
	if err := c.call(ctx, http.MethodDelete, routeSnapshot(vmID, id), nil, &o); err != nil {
		return err
	}
	return c.waitIfTask(ctx, o)
}

// RevertSnapshot reverts a VM to a snapshot and waits for it.
func (c *Client) RevertSnapshot(ctx context.Context, vmID, id string) error {
	var o obj
	if err := c.call(ctx, http.MethodPost, routeSnapshotRevert(vmID, id), map[string]any{}, &o); err != nil {
		return err
	}
	return c.waitIfTask(ctx, o)
}

// waitIfTask waits for the task a reply names, if it names one.
func (c *Client) waitIfTask(ctx context.Context, reply obj) error {
	id := reply.str(fieldTask)
	if id == "" {
		return nil
	}
	return c.WaitTask(ctx, id)
}

// ErrTaskFailed means Virtualization Station reported a task failed.
var ErrTaskFailed = errors.New("qvs: the task failed")

// WaitTask polls a background task until it finishes.
func (c *Client) WaitTask(ctx context.Context, id string) error {
	deadline := time.Now().Add(c.TaskTimeout)
	for {
		var o obj
		if err := c.call(ctx, http.MethodGet, routeTask(id), nil, &o); err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
				return nil // finished tasks are cleaned up (probe 2: the task table is empty afterwards)
			}
			return err
		}
		switch s := strings.ToLower(o.str(fieldTaskEnd)); {
		case s == "" || strings.Contains(s, "done") || strings.Contains(s, "success") || strings.Contains(s, "finish") || strings.Contains(s, "complete"):
			return nil
		case strings.Contains(s, "fail") || strings.Contains(s, "error") || strings.Contains(s, "abort") || strings.Contains(s, "cancel"):
			return fmt.Errorf("%w (task %s: %s)", ErrTaskFailed, id, s)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("qvs: task %s didn't finish within %s", id, c.TaskTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollEvery):
		}
	}
}
