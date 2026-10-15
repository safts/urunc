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
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/moby/sys/user"
)

// guestEnvWithHome returns the guest's environment with HOME set the way runc
// sets it for a container: when the spec leaves HOME unset or empty, it is the
// home directory of uid in the container image's /etc/passwd, or "/" when the
// file or the user is missing or the entry has no home. Neither the CRI nor
// the OCI spec fills HOME in; runc does at exec time, and programs such as git
// refuse to run without it. rootfs is the host path of the container rootfs;
// /etc/passwd is resolved inside it. env itself is never modified.
func guestEnvWithHome(env []string, rootfs string, uid uint32) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") && e != "HOME=" {
			return append(out[:0], env...)
		}
	}
	for _, e := range env {
		if e != "HOME=" {
			out = append(out, e)
		}
	}
	return append(out, "HOME="+passwdHome(rootfs, uid))
}

// passwdHome looks uid up in the /etc/passwd of rootfs, defaulting to "/".
func passwdHome(rootfs string, uid uint32) string {
	path, err := securejoin.SecureJoin(rootfs, "/etc/passwd")
	if err != nil {
		return "/"
	}
	users, err := user.ParsePasswdFileFilter(path, func(u user.User) bool {
		return u.Uid >= 0 && uint32(u.Uid) == uid // nolint:gosec
	})
	if err != nil || len(users) == 0 || users[0].Home == "" {
		return "/"
	}
	return users[0].Home
}
