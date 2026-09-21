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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
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

// The monitor runs as the container's user, and setting a tap's MTU needs
// CAP_NET_ADMIN, which that user does not have for an image that runs as
// anyone but root. urunc sets the MTU itself while it still can, so asking
// Cloud Hypervisor to set it again fails the boot for every non-root image
// and gains nothing when it works.
func TestCloudHypervisorNetLeavesTheTapMTUAlone(t *testing.T) {
	ch := &CloudHypervisor{binary: CloudHypervisorBinary, binaryPath: "/usr/bin/cloud-hypervisor"}
	argv, err := ch.BuildExecCmd(types.ExecArgs{
		UnikernelPath: "/rootfs/unikernel.bin",
		Command:       "init=/bin/sh",
		Net:           types.NetDevParams{TapDev: "tap0", MAC: "52:54:00:12:34:56", MTU: 1500},
	}, &fakeUnikernel{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "tap=tap0") || !strings.Contains(joined, "mac=52:54:00:12:34:56") {
		t.Errorf("expected the tap and mac in:\n%s", joined)
	}
	if strings.Contains(joined, "mtu=") {
		t.Errorf("mtu= makes Cloud Hypervisor call SIOCSIFMTU, which a non-root monitor "+
			"cannot do:\n%s", joined)
	}
}
