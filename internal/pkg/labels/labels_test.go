// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package labels

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gotest.tools/v3/assert"
)

func TestParse(t *testing.T) {
	tests := []struct {
		args    string
		want    *Label
		wantErr bool
	}{
		{
			args: "ubuntu:docker://node:18",
			want: &Label{
				Name:   "ubuntu",
				Schema: "docker",
				Arg:    "//node:18",
			},
			wantErr: false,
		},
		{
			args: "ubuntu:host",
			want: &Label{
				Name:   "ubuntu",
				Schema: "host",
				Arg:    "",
			},
			wantErr: false,
		},
		{
			args: "ubuntu",
			want: &Label{
				Name:   "ubuntu",
				Schema: "host",
				Arg:    "",
			},
			wantErr: false,
		},
		{
			args:    "ubuntu:vm:ubuntu-18.04",
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			got, err := Parse(tt.args)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.DeepEqual(t, got, tt.want)
		})
	}
}

// mustParse parses the given label strings, failing the test on any error.
func mustParse(t *testing.T, strs ...string) Labels {
	t.Helper()
	ls := make(Labels, 0, len(strs))
	for _, s := range strs {
		l, err := Parse(s)
		require.NoError(t, err)
		ls = append(ls, l)
	}
	return ls
}

func TestRequireDocker(t *testing.T) {
	tests := []struct {
		name string
		strs []string
		want bool
	}{
		{"empty", nil, false},
		{"only host", []string{"ubuntu:host", "self-hosted"}, false},
		{"has docker", []string{"ubuntu:host", "ubuntu:docker://node:18"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, mustParse(t, tt.strs...).RequireDocker())
		})
	}
}

func TestPickPlatform(t *testing.T) {
	ls := mustParse(t,
		"ubuntu:docker://node:18",
		"self-hosted:host",
	)

	tests := []struct {
		name   string
		runsOn []string
		want   string
	}{
		{"docker strips leading slashes", []string{"ubuntu"}, "node:18"},
		{"host maps to self-hosted marker", []string{"self-hosted"}, "-self-hosted"},
		{"first match wins", []string{"self-hosted", "ubuntu"}, "-self-hosted"},
		{"unknown falls back to default", []string{"windows"}, "docker.gitea.com/runner-images:ubuntu-latest"},
		{"no runsOn falls back to default", nil, "docker.gitea.com/runner-images:ubuntu-latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ls.PickPlatform(tt.runsOn))
		})
	}
}

func TestNames(t *testing.T) {
	ls := mustParse(t, "ubuntu:docker://node:18", "self-hosted:host")
	require.Equal(t, []string{"ubuntu", "self-hosted"}, ls.Names())
	require.Empty(t, Labels{}.Names())
}

func TestToStrings(t *testing.T) {
	ls := mustParse(t,
		"ubuntu:docker://node:18",
		"self-hosted:host",
		"bare",
	)
	require.Equal(t, []string{
		"ubuntu:docker://node:18",
		"self-hosted:host",
		"bare:host",
	}, ls.ToStrings())
}
