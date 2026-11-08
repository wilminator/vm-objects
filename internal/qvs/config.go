// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package qvs talks to QNAP Virtualization Station's own API (a Django
// application under /qvs/ on the QTS web server), for what libvirt alone
// mustn't do on a QNAP: Virtualization Station keeps its own record of
// every VM and snapshot, so changes go through it. vmobj reads from
// libvirt and changes things through here.
//
// The routes and field names come from Virtualization Station 4.2's code,
// read without running it; routes.go holds them all, so confirming them on
// a live system changes one file.
package qvs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Config is how to reach Virtualization Station and log in.
type Config struct {
	URL      string // QTS's web address as seen from where vmobj runs, e.g. http://127.0.0.1:8080
	Username string
	Password string
}

// LoadConfig reads a credentials file:
//
//	[qvs]
//	url = http://127.0.0.1:8080
//	username = vmobj
//	password = ...
//
// It refuses a file that anyone but its owner can read: it holds a
// password.
func LoadConfig(path string) (*Config, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s can be read by others (mode %o); it holds a password: chmod 600 it", path, fi.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &Config{}
	section := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "qvs" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s line %d: expected key = value", path, n)
		}
		switch strings.TrimSpace(k) {
		case "url":
			c.URL = strings.TrimRight(strings.TrimSpace(v), "/")
		case "username":
			c.Username = strings.TrimSpace(v)
		case "password":
			c.Password = strings.TrimSpace(v)
		default:
			return nil, fmt.Errorf("%s line %d: unknown key %q", path, n, strings.TrimSpace(k))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if c.URL == "" || c.Username == "" || c.Password == "" {
		return nil, errors.New(path + ": the [qvs] section needs url, username and password")
	}
	return c, nil
}
