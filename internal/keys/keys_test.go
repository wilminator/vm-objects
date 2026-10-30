// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package keys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmobj.key")
	if err := Generate(path); err != nil {
		t.Fatal(err)
	}
	if err := Generate(path); err == nil {
		t.Fatal("Generate overwrote an existing key")
	}
	a, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Load(path)
	if a != b || a.Data == a.ID {
		t.Fatal("derived keys are not stable or not separate")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a world-readable key file")
	}
}
