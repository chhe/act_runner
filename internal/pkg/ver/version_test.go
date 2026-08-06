// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package ver

import "testing"

func TestVersion(t *testing.T) {
	if got := Version(); got == "" || got == "(devel)" {
		t.Errorf("Version() = %q, want a concrete version", got)
	}

	version = "1.2.3"
	defer func() { version = "" }()
	if got := Version(); got != version {
		t.Errorf("Version() = %q, want %q", got, version)
	}
}
