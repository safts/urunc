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

package unikontainers

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGuestExitCode(t *testing.T) {
	t.Parallel()

	exited := func(code int) syscall.WaitStatus { return syscall.WaitStatus(code << 8) }
	for _, tc := range []struct {
		vmm    syscall.WaitStatus
		record string
		want   int
	}{
		{exited(0), "EXIT:3\n", 3},
		{exited(0), "SIGNAL:11\n", 139},
		{exited(0), "EXIT:0\n", 0},
		{exited(0), "EXIT:7", 7},
		{exited(0), "", 0},
		{exited(0), "garbage", 0},
		{exited(0), "EXIT:256", 0},
		{exited(0), "EXIT:-1", 0},
		{exited(0), "SIGNAL:0", 0},
		{exited(0), "SIGNAL:128", 0},
		{exited(1), "EXIT:3", 1},
		{syscall.WaitStatus(syscall.SIGKILL), "EXIT:3", 137},
	} {
		assert.Equal(t, tc.want, guestExitCode(tc.vmm, []byte(tc.record)), "%v %q", tc.vmm, tc.record)
	}
}
