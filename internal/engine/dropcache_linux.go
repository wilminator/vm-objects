// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"os"
	"syscall"
)

// dropCache asks the kernel to forget its cached pages of f, so the next
// read comes from the disk. It's advice: a failure is ignored.
func dropCache(f *os.File) {
	const fadvDontNeed = 4 // POSIX_FADV_DONTNEED
	_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, fadvDontNeed, 0, 0)
}
