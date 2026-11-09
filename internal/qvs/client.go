// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package qvs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Client is a logged-in session with Virtualization Station. The account
// is a QTS administrator (Virtualization Station gives other users no way
// to create VMs), so the client only ever calls the closed set of routes
// in routes.go, with typed ids.
type Client struct {
	cfg  *Config
	base *url.URL
	http *http.Client
	sid  string
	// PollEvery and TaskTimeout govern waiting for Virtualization
	// Station to finish something (a snapshot, a shutdown).
	PollEvery   time.Duration
	TaskTimeout time.Duration
}

// New returns a client; call Login before anything else.
func New(cfg *Config) (*Client, error) {
	base, err := checkURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CA != "" {
		pem, err := os.ReadFile(cfg.CA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("qvs: %s holds no PEM certificate", cfg.CA)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	hc := &http.Client{
		Jar: jar, Transport: tr, Timeout: 2 * time.Minute,
		// Never follow a redirect: a 307 or 308 would resend the login
		// form, password included, wherever it pointed. The API never
		// needs one.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Client{cfg: cfg, base: base, http: hc, PollEvery: 2 * time.Second, TaskTimeout: 30 * time.Minute}, nil
}

// checkURL allows https anywhere, and plain http only to this machine:
// QTS takes the password merely base64-encoded, and the session id is a
// whole administrator session.
func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("qvs: url %q must be just scheme://host[:port]", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		if h != "127.0.0.1" && h != "::1" && h != "localhost" {
			return nil, fmt.Errorf("qvs: url %q: plain http only to this machine (127.0.0.1); use https, with ca = the QTS certificate if it's self-signed", raw)
		}
	default:
		return nil, fmt.Errorf("qvs: url %q must be http or https", raw)
	}
	u.Path = ""
	return u, nil
}

// ErrNotLoggedIn means a login was refused or the session ended.
var ErrNotLoggedIn = errors.New("qvs: not logged in")

type qtsLogin struct {
	AuthPassed string `xml:"authPassed"`
	AuthSid    string `xml:"authSid"`
	Need2SV    string `xml:"need_2sv"`
}

// Login logs in to QTS, which gives a session id, and hands that to
// Virtualization Station as its NAS_SID cookie, which its middleware
// authenticates every request with. The password goes only in the POST
// body, never in a URL or a message.
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
	if status != http.StatusOK || xml.Unmarshal(body, &r) != nil {
		return fmt.Errorf("qts login: unexpected reply (HTTP %d)", status)
	}
	switch {
	case strings.TrimSpace(r.Need2SV) == "1":
		return fmt.Errorf("%w: the account has 2-step verification on; vmobj's account needs it off", ErrNotLoggedIn)
	case strings.TrimSpace(r.AuthPassed) != "1" || strings.TrimSpace(r.AuthSid) == "":
		return fmt.Errorf("%w: QTS refused the username or password for %q", ErrNotLoggedIn, c.cfg.Username)
	}
	c.sid = strings.TrimSpace(r.AuthSid)
	c.http.Jar.SetCookies(c.base, []*http.Cookie{{Name: sidCookie, Value: c.sid, Path: "/",
		Secure: c.base.Scheme == "https", HttpOnly: true}})
	var check struct{}
	if err := c.call(ctx, http.MethodGet, routeCheck, nil, &check); err != nil {
		if errors.Is(err, ErrNotLoggedIn) {
			return fmt.Errorf("%w: Virtualization Station doesn't know %q; it must be in the QTS administrators group", ErrNotLoggedIn, c.cfg.Username)
		}
		return err
	}
	return nil
}

// Logout ends both sessions. QTS wants the session id in the logout URL;
// vmobj never logs that URL. Errors are ignored: it's cleanup.
func (c *Client) Logout(ctx context.Context) {
	if c.sid == "" {
		return
	}
	c.raw(ctx, http.MethodPost, routeLogout, "application/json", nil)
	c.raw(ctx, http.MethodGet, routeQTSLogout+"?sid="+url.QueryEscape(c.sid), "", nil)
	c.sid = ""
}

func (c *Client) csrf() string {
	for _, ck := range c.http.Jar.Cookies(c.base) {
		if ck.Name == "csrftoken" {
			return ck.Value
		}
	}
	return ""
}

// raw sends one request to a route from routes.go and returns the body
// and status. Django wants the CSRF token in a header (and a Referer) on
// anything that changes something, when it has issued one.
func (c *Client) raw(ctx context.Context, method, route, ctype string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+route, rd)
	if err != nil {
		return nil, 0, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", c.base.String()+"/qvs/")
	if method != http.MethodGet && method != http.MethodHead {
		if t := c.csrf(); t != "" {
			req.Header.Set("X-CSRFToken", t)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, redactErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, fmt.Errorf("qvs %s %s: refused a redirect (HTTP %d); check url in the credentials file", method, route, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return b, resp.StatusCode, err
}

// redactErr keeps a session id out of transport errors, which quote the
// URL (only the QTS logout URL carries one).
func redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// APIError is a request Virtualization Station refused. Detail is the
// server's text, cleaned for a terminal and with secrets redacted.
type APIError struct {
	Method, Route string
	HTTP, Status  int
	Detail        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("qvs %s %s: HTTP %d, status %d: %s", e.Method, e.Route, e.HTTP, e.Status, e.Detail)
}

var secretish = regexp.MustCompile(`(?i)("?[a-z_]*(sid|token|csrf|passw(or)?d|secret)[a-z_]*"?\s*[:=]\s*)("[^"]*"|[^\s,}&]+)`)

// clean makes server text safe to print: no control characters or
// escape sequences (which could rewrite a terminal or forge log lines),
// anything that looks like a secret redacted, and at most 300 characters.
func clean(s string) string {
	s = secretish.ReplaceAllString(s, `${1}[redacted]`)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			// dropped: control characters, ESC included
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len([]rune(out)) > 300 {
		out = string([]rune(out)[:300]) + "…"
	}
	return out
}

type envelope struct {
	Status *int            `json:"status"`
	Data   json.RawMessage `json:"data"`
	Detail string          `json:"detail"`
}

// call sends JSON to a route and decodes the reply's data into out.
func (c *Client) call(ctx context.Context, method, route string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	b, httpStatus, err := c.raw(ctx, method, route, "application/json", body)
	if err != nil {
		return fmt.Errorf("qvs %s %s: %w", method, route, err)
	}
	var env envelope
	jerr := json.Unmarshal(b, &env)
	status := -1
	if jerr == nil && env.Status != nil {
		status = *env.Status
	}
	if status == statusNotAuthenticated || httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden {
		return fmt.Errorf("%w (qvs %s %s)", ErrNotLoggedIn, method, route)
	}
	if status == statusQueued && httpStatus < 300 {
		// A write VS runs in the background: data is the task. Wait for
		// it here, so every caller sees a finished change.
		var t task
		if err := json.Unmarshal(env.Data, &t); err != nil || t.ID == 0 {
			return &APIError{Method: method, Route: route, HTTP: httpStatus, Status: status, Detail: "queued, but the reply names no task"}
		}
		if err := c.waitTask(ctx, t.ID); err != nil {
			return fmt.Errorf("qvs %s %s: %w", method, route, err)
		}
		return nil
	}
	if httpStatus >= 300 || (status != statusOK && status != -1) || (jerr != nil && len(bytes.TrimSpace(b)) > 0) {
		detail := env.Detail
		if detail == "" {
			detail = string(b)
		}
		return &APIError{Method: method, Route: route, HTTP: httpStatus, Status: status, Detail: clean(detail)}
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("qvs %s %s: the reply's data isn't what vmobj expects: %w", method, route, err)
	}
	return nil
}

// VM is Virtualization Station's view of a VM.
type VM struct {
	ID         ID     `json:"id"`
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	PowerState string `json:"power_state"`
}

// ListVMs lists every VM.
func (c *Client) ListVMs(ctx context.Context) ([]VM, error) {
	var vms []VM
	if err := c.call(ctx, http.MethodGet, routeVMs, nil, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

// FindVM finds a VM by UUID (preferred: names can repeat) or name.
func (c *Client) FindVM(ctx context.Context, uuid, name string) (*VM, error) {
	vms, err := c.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	if uuid != "" {
		for i := range vms {
			if strings.EqualFold(vms[i].UUID, uuid) {
				return &vms[i], nil
			}
		}
		return nil, fmt.Errorf("qvs: no VM has UUID %q", uuid)
	}
	var found *VM
	for i := range vms {
		if vms[i].Name == name {
			if found != nil {
				return nil, fmt.Errorf("qvs: two VMs are named %q; use its UUID", name)
			}
			found = &vms[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("qvs: no VM is named %q", name)
	}
	return found, nil
}

// State reads a VM's state: running, stopped or paused (or VS's word).
func (c *Client) State(ctx context.Context, id ID) (string, error) {
	var vm VM
	if err := c.call(ctx, http.MethodGet, routeVM(id), nil, &vm); err != nil {
		return "", err
	}
	return NormalState(vm.PowerState), nil
}

// NormalState maps Virtualization Station's power_state ("running",
// "stop", ...) to running, stopped or paused.
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

// Act runs one of the allowlisted power actions, waiting for it when VS
// queues it.
func (c *Client) Act(ctx context.Context, id ID, a Action) error {
	if !a.valid() {
		return fmt.Errorf("qvs: %q is not an action vmobj sends", a)
	}
	return c.call(ctx, http.MethodPost, routeVMAction(id, a), map[string]any{}, nil)
}

// Snapshot is Virtualization Station's view of a snapshot.
type Snapshot struct {
	ID          ID     // its epoch, which names its overlay and record
	Name        string // the name shown in the UI
	Description string
	Created     time.Time // UTC
	Locked      bool
	CreatedBy   string // user, scheduling
}

type snapshotJSON struct {
	ID           ID     `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	CreationTime string `json:"creation_time"` // "2026-11-08T02:01:27", UTC
	LockSnapshot string `json:"lock_snapshot"` // "unlock", "lock"
	CreatedBy    string `json:"created_by"`
}

// ListSnapshots lists a VM's snapshots.
func (c *Client) ListSnapshots(ctx context.Context, vm ID) ([]Snapshot, error) {
	var raw []snapshotJSON
	if err := c.call(ctx, http.MethodGet, routeSnapshots(vm), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		t, _ := time.Parse("2006-01-02T15:04:05", s.CreationTime)
		if t.IsZero() {
			t = time.Unix(int64(s.ID), 0).UTC() // the id is the epoch
		}
		out = append(out, Snapshot{ID: s.ID, Name: s.Name, Description: s.Description, Created: t.UTC(),
			Locked: s.LockSnapshot == "lock", CreatedBy: s.CreatedBy})
	}
	return out, nil
}

// CreateSnapshot takes a snapshot as the UI does, then waits until it is
// listed, and returns it.
func (c *Client) CreateSnapshot(ctx context.Context, vm ID, name, description string) (*Snapshot, error) {
	if name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return nil, fmt.Errorf("qvs: snapshot name %q", name)
	}
	in := snapshotCreate{Name: name, Description: description, LockSnapshot: "unlock"}
	if err := c.call(ctx, http.MethodPost, routeSnapshots(vm), in, nil); err != nil {
		return nil, err
	}
	var got *Snapshot
	err := c.until(ctx, func() (bool, error) {
		snaps, err := c.ListSnapshots(ctx, vm)
		for i := range snaps {
			if snaps[i].Name == name {
				got = &snaps[i]
				return true, nil
			}
		}
		return false, err
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	return got, nil
}

// DeleteSnapshot deletes a snapshot, unlocking it first if it's locked,
// and waits until it's gone from the list.
func (c *Client) DeleteSnapshot(ctx context.Context, vm, snap ID) error {
	snaps, err := c.ListSnapshots(ctx, vm)
	if err != nil {
		return err
	}
	for _, s := range snaps {
		if s.ID == snap && s.Locked {
			if err := c.call(ctx, http.MethodPost, routeSnapshotLock(vm, snap), snapshotUnlock{LockStatus: "unlock"}, nil); err != nil {
				return fmt.Errorf("unlocking snapshot %d: %w", snap, err)
			}
		}
	}
	if err := c.call(ctx, http.MethodDelete, routeSnapshot(vm, snap), nil, nil); err != nil {
		return err
	}
	return c.until(ctx, func() (bool, error) {
		snaps, err := c.ListSnapshots(ctx, vm)
		for _, s := range snaps {
			if s.ID == snap {
				return false, err
			}
		}
		return err == nil, err
	})
}

// RevertSnapshot reverts a VM to a snapshot and waits for it.
func (c *Client) RevertSnapshot(ctx context.Context, vm, snap ID) error {
	return c.call(ctx, http.MethodPost, routeSnapshotRevert(vm, snap), map[string]any{}, nil)
}

// ErrTaskFailed means Virtualization Station reported a task failed.
var ErrTaskFailed = errors.New("qvs: the task failed")

// waitTask polls a background task until it finishes. A task VS has
// already cleaned up (404) counts as finished.
func (c *Client) waitTask(ctx context.Context, id ID) error {
	return c.until(ctx, func() (bool, error) {
		var t task
		if err := c.call(ctx, http.MethodGet, routeTask(id), nil, &t); err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.HTTP == http.StatusNotFound {
				return true, nil
			}
			return false, err
		}
		s := strings.ToLower(t.State)
		switch {
		case t.Failed, strings.Contains(s, "fail"), strings.Contains(s, "error"), strings.Contains(s, "abort"), strings.Contains(s, "cancel"):
			return false, fmt.Errorf("%w (task %d: %s)", ErrTaskFailed, id, clean(t.State))
		case strings.Contains(s, "finish"), strings.Contains(s, "done"), strings.Contains(s, "success"), strings.Contains(s, "complete"):
			return true, nil
		}
		return false, nil // waiting, running...
	})
}

// until polls f until it reports done, fails, or TaskTimeout passes.
func (c *Client) until(ctx context.Context, f func() (bool, error)) error {
	deadline := time.Now().Add(c.TaskTimeout)
	for {
		done, err := f()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("qvs: not done within %s", c.TaskTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollEvery):
		}
	}
}
