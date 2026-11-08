// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"context"
	"net"
	"testing"
)

func TestParseS3IP(t *testing.T) {
	for addr, want := range map[string]string{
		"s3://b/p?region=us-east-2":      "",
		"s3://b/p?region=us-east-2&ip=4": "4",
		"s3://b/p?region=us-east-2&ip=6": "6",
	} {
		loc, err := ParseS3(addr)
		if err != nil || loc.IP != want {
			t.Errorf("%s: ip %q, %v", addr, loc.IP, err)
		}
	}
	for _, bad := range []string{"s3://b?ip=v4", "s3://b?ip=46", "s3://b?ip=ipv6"} {
		if _, err := ParseS3(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// DialIP connects only over the version asked for: to a server listening
// on 127.0.0.1 alone, "localhost" works over IPv4 and is refused over
// IPv6, even where localhost also resolves to ::1.
func TestDialIP(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	ctx := context.Background()
	c, err := DialIP("4")(ctx, "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("ip=4: %v", err)
	}
	if ip := c.RemoteAddr().(*net.TCPAddr).IP; ip.To4() == nil {
		t.Fatalf("ip=4 connected to %s", ip)
	}
	c.Close()
	if c, err := DialIP("6")(ctx, "tcp", net.JoinHostPort("localhost", port)); err == nil {
		t.Fatalf("ip=6 reached an IPv4-only server at %s", c.RemoteAddr())
	}
}
