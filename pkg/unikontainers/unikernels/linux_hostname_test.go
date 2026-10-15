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

package unikernels

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guest's hostname is the container's (spec.hostname, the pod name under
// Kubernetes), as runc gives it through the UTS namespace: kubelet's
// /etc/hosts maps that name, so looking it up never leaves the guest.
func TestLinuxCommandStringHostname(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{name: "pod name", hostname: "issue-repo-42-0", want: "issue-repo-42-0"},
		{name: "fqdn", hostname: "web-0.web.ns.svc", want: "web-0.web.ns.svc"},
		{name: "unset", hostname: "", want: "urunc"},
		{name: "colon would split ip=", hostname: "a:b", want: "urunc"},
		{name: "space would split the command line", hostname: "a b", want: "urunc"},
		{name: "longer than the kernel's 64", hostname: strings.Repeat("a", 65), want: "urunc"},
		{name: "64 is the limit", hostname: strings.Repeat("a", 64), want: strings.Repeat("a", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := &Linux{
				App:      "/bin/app",
				Monitor:  "qemu",
				Hostname: tt.hostname,
				Net:      LinuxNet{Address: "10.0.0.2", Gateway: "10.0.0.1", Mask: "255.255.255.0"},
			}
			cmdline, err := l.CommandString()
			require.NoError(t, err)
			assert.Contains(t, cmdline, " ip=10.0.0.2::10.0.0.1:255.255.255.0:"+tt.want+":eth0:off")
		})
	}
}
