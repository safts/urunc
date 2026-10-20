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
	"errors"
	"os"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// execExit ends the exec proxy. Tests replace it.
var execExit = os.Exit

// execInitGoneCode is the exit code of an exec ended by its container's init
// exiting: 128+SIGKILL, what a runc exec killed with its pid namespace gets.
const execInitGoneCode = 128 + int(unix.SIGKILL)

// exitWithInit ends this exec proxy when the container's init (pid) exits.
//
// containerd's runc shim publishes a container's exit only once every exec of
// it has exited, and it relies on the kernel for that: a runc exec lives in
// the container's pid namespace and dies with its init. A urunc exec proxy is
// a host process outside that namespace, so nothing ends it with the VM. One
// that outlives the VM (for instance blocked writing output nobody reads)
// leaves the container "running" and its stop waiting forever. So the proxy
// watches the init itself, through a pidfd, which also reports an exited init
// that is still a zombie.
func exitWithInit(pid int) {
	if pid <= 0 {
		return
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		logrus.Infof("container init (pid %d) is gone, ending exec session", pid)
		execExit(execInitGoneCode)
		return
	}
	if err != nil {
		logrus.WithError(err).Warnf("cannot watch container init (pid %d); this exec will not end with the container", pid)
		return
	}
	go func() {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // fds fit in int32
		for {
			_, err := unix.Poll(fds, -1)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EINTR) {
				logrus.WithError(err).Warnf("stopped watching container init (pid %d)", pid)
				return
			}
		}
		logrus.Infof("container init (pid %d) exited, ending exec session", pid)
		execExit(execInitGoneCode)
	}()
}
