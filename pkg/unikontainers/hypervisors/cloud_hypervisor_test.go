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

package hypervisors

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

const testCHBinary = "/usr/bin/cloud-hypervisor"

// TestCloudHypervisorBuildExecCmdSocket verifies Cloud Hypervisor emits
// --api-socket only when socket_path is set.
func TestCloudHypervisorBuildExecCmdSocket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           types.ExecArgs
		mustContain    []string
		mustNotContain []string
	}{
		{
			name: "configured SocketPath renders --api-socket on that path",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
				SocketPath:    "/run/urunc/ch.sock",
			},
			mustContain: []string{"--api-socket path=/run/urunc/ch.sock"},
		},
		{
			name: "unset SocketPath omits --api-socket",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
				ContainerID:   "abc123",
			},
			mustNotContain: []string{"--api-socket"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ch := &CloudHypervisor{binary: CloudHypervisorBinary, binaryPath: testCHBinary}
			out, err := ch.BuildExecCmd(tt.args, &fakeUnikernel{})
			assert.NoError(t, err)
			assert.NotEmpty(t, out)

			assert.Equal(t, testCHBinary, out[0], "binary path must be the first element")
			joined := strings.Join(out, " ")

			for _, want := range tt.mustContain {
				assert.Contains(t, joined, want, "expected %q to be present", want)
			}
			for _, notWant := range tt.mustNotContain {
				assert.NotContains(t, joined, notWant, "expected %q to be absent", notWant)
			}
		})
	}
}

// TestCloudHypervisorSignalTermEscalates verifies a SIGTERM that the monitor
// does not act on ends in a SIGKILL after a bounded wait, so kill returns with
// the process gone instead of leaving the stop to the guest or the VMM.
func TestCloudHypervisorSignalTermEscalates(t *testing.T) {
	old := chTermTimeout
	chTermTimeout = 300 * time.Millisecond
	t.Cleanup(func() { chTermTimeout = old })

	// A stand-in for a VMM whose shutdown hangs: it ignores SIGTERM.
	cmd := exec.Command("sh", "-c", `trap "" TERM; echo ready; read x`)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// Wait until the trap is installed before signalling.
	_, err = stdout.Read(make([]byte, 6))
	require.NoError(t, err)
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	ch := &CloudHypervisor{}
	require.NoError(t, ch.Signal(cmd.Process.Pid, unix.SIGTERM))

	select {
	case <-exited:
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process still alive after SIGTERM returned")
	}
}

// TestCloudHypervisorSignalTermExited verifies SIGTERM to a monitor that is
// already gone is not an error.
func TestCloudHypervisorSignalTermExited(t *testing.T) {
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())

	ch := &CloudHypervisor{}
	assert.NoError(t, ch.Signal(cmd.Process.Pid, unix.SIGTERM))
}
