// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func fakeBinary(t *testing.T, dir string, content string) string {
	t.Helper()
	p := filepath.Join(dir, "vmobj-src")
	if err := os.WriteFile(p, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallSelf(t *testing.T) {
	src := fakeBinary(t, t.TempDir(), "binary v1")
	out := t.TempDir()

	// A fresh install: the same bytes, mode 0755, nothing left behind.
	res, err := installSelf(src, out, "vmobj")
	if err != nil {
		t.Fatal(err)
	}
	if res.replaced || res.unchanged || res.path != filepath.Join(out, "vmobj") {
		t.Fatalf("fresh install: %+v", res)
	}
	got, _ := os.ReadFile(res.path)
	if !bytes.Equal(got, []byte("binary v1")) {
		t.Fatal("installed file differs from the source")
	}
	if info, _ := os.Stat(res.path); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v, want 0755", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(out); len(entries) != 1 {
		t.Fatalf("install left %d files in the folder, want 1", len(entries))
	}

	// The same binary again writes nothing.
	if res, err := installSelf(src, out, "vmobj"); err != nil || !res.unchanged {
		t.Fatalf("reinstall of the same binary: %+v %v", res, err)
	}

	// A newer binary replaces the old one.
	src2 := fakeBinary(t, t.TempDir(), "binary v2, longer")
	res, err = installSelf(src2, out, "vmobj")
	if err != nil || !res.replaced {
		t.Fatalf("upgrade: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(res.path); string(got) != "binary v2, longer" {
		t.Fatalf("after upgrade the file holds %q", got)
	}

	// --name picks the file name.
	if res, err := installSelf(src, out, "vmobj-test"); err != nil || res.path != filepath.Join(out, "vmobj-test") {
		t.Fatalf("--name: %+v %v", res, err)
	}
}

func TestInstallSelfOntoItself(t *testing.T) {
	dir := t.TempDir()
	src := fakeBinary(t, dir, "running binary")
	if err := os.Rename(src, filepath.Join(dir, "vmobj")); err != nil {
		t.Fatal(err)
	}
	res, err := installSelf(filepath.Join(dir, "vmobj"), dir, "vmobj")
	if err != nil || !res.unchanged {
		t.Fatalf("installing a binary onto itself: %+v %v", res, err)
	}
}

func TestInstallSelfRefusals(t *testing.T) {
	src := fakeBinary(t, t.TempDir(), "x")
	out := t.TempDir()
	for _, tc := range []struct{ dir, name string }{
		{filepath.Join(out, "missing"), "vmobj"}, // folder doesn't exist
		{src, "vmobj"},                           // not a folder
		{out, "../vmobj"},                        // name escapes the folder
		{out, ""},
	} {
		if _, err := installSelf(src, tc.dir, tc.name); err == nil {
			t.Errorf("install to %q as %q was allowed", tc.dir, tc.name)
		}
	}
	if err := os.Mkdir(filepath.Join(out, "vmobj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installSelf(src, out, "vmobj"); err == nil {
		t.Error("install over a folder was allowed")
	}
}

func TestDescribeVMFiles(t *testing.T) {
	for _, tc := range []struct {
		nvram, tpm int
		want       string
	}{
		{0, 2, "TPM state (2 files), no NVRAM"},
		{1, 2, "NVRAM (1 file), TPM state (2 files)"},
		{1, 0, "NVRAM (1 file), no TPM"},
		{0, 0, "none (no NVRAM or TPM state)"},
	} {
		if got := describeVMFiles(tc.nvram, tc.tpm); got != tc.want {
			t.Errorf("%d nvram, %d tpm: %q, want %q", tc.nvram, tc.tpm, got, tc.want)
		}
	}
}
