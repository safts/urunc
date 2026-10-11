// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/urunc-dev/urunc/pkg/unikontainers"
)

var monitorLogArg = "/proc/self/fd/" + strconv.Itoa(unikontainers.LogFD)

// hasLogArg reports whether args carry "--log <monitorLogArg>" before "monitor".
func hasLogArg(args []string) bool {
	i := slices.Index(args, "--log")
	m := slices.Index(args, monitorArgv)
	return i >= 0 && i+1 < len(args) && args[i+1] == monitorLogArg && i < m
}

func TestBuildMonitorArgs(t *testing.T) {
	args := buildMonitorArgs("id", "json", true)
	assert.True(t, hasLogArg(args), "%v", args)
	assert.Equal(t, []string{monitorArgv, "id"}, args[len(args)-2:])

	args = buildMonitorArgs("id", "json", false)
	assert.NotContains(t, args, "--log")
}

// Not parallel: it swaps logrus's global output.
func TestMonitorProcessPassesLogFile(t *testing.T) {
	out := logrus.StandardLogger().Out
	t.Cleanup(func() { logrus.SetOutput(out) })

	u := &unikontainers.Unikontainer{
		State: &specs.State{ID: "id"},
		Spec:  &specs.Spec{Process: &specs.Process{}},
	}
	ready, err := os.CreateTemp(t.TempDir(), "ready")
	require.NoError(t, err)
	defer ready.Close()
	cmd := &cli.Command{}

	t.Run("log file", func(t *testing.T) {
		logFile, err := os.Create(filepath.Join(t.TempDir(), "log"))
		require.NoError(t, err)
		defer logFile.Close()
		logrus.SetOutput(logFile)

		p, err := monitorProcess(cmd, u, ready)
		require.NoError(t, err)
		assert.Equal(t, []*os.File{ready, logFile}, p.ExtraFiles)
		assert.True(t, hasLogArg(p.Args), "%v", p.Args)
	})

	t.Run("stderr", func(t *testing.T) {
		logrus.SetOutput(os.Stderr)

		p, err := monitorProcess(cmd, u, ready)
		require.NoError(t, err)
		assert.Equal(t, []*os.File{ready}, p.ExtraFiles)
		assert.NotContains(t, p.Args, "--log")
	})
}
