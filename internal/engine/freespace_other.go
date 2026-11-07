// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !unix

package engine

// freeSpace isn't known on this system; the journal relies on write errors.
func freeSpace(string) (int64, bool) { return 0, false }
