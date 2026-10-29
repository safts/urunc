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
	"fmt"
	"strconv"
	"strings"

	"github.com/urunc-dev/urunc/pkg/network"
)

// annotPodServices names the containers of a pod that run as service VMs
// next to its primary VM, as "<container>:<port>[:<port>...]" entries,
// comma separated; the i-th entry is service i, reached by the primary at
// network.ServiceAddress(i), and its ports are the ones the pod's address
// sends to it. It is a pod annotation, so every container of the pod sees
// it; a container it does not name is the pod's primary.
const annotPodServices = "com.urunc.unikernel.podServices"

// podNetwork is the container's part in its pod's network, or nil when the
// pod runs no service VMs.
func podNetwork(annotations map[string]string) (*network.PodNetwork, error) {
	v := annotations[annotPodServices]
	if v == "" {
		return nil, nil
	}
	entries := strings.Split(v, ",")
	if len(entries) > network.MaxPodServices {
		return nil, fmt.Errorf("%s: %d services, at most %d", annotPodServices, len(entries), network.MaxPodServices)
	}
	self := annotations[annotCRICntrName]
	pn := &network.PodNetwork{}
	names := map[string]bool{}
	ports := map[uint64]bool{}
	for i, e := range entries {
		fields := strings.Split(e, ":")
		name := fields[0]
		if name == "" || names[name] {
			return nil, fmt.Errorf("%s: entry %d: empty or repeated container name %q", annotPodServices, i+1, name)
		}
		names[name] = true
		var ps []uint16
		for _, f := range fields[1:] {
			p, err := strconv.ParseUint(f, 10, 16)
			if err != nil || p == 0 || ports[p] {
				return nil, fmt.Errorf("%s: %s: bad or repeated port %q", annotPodServices, name, f)
			}
			ports[p] = true
			ps = append(ps, uint16(p))
		}
		if name == self {
			pn.Service, pn.Ports = i+1, ps
		}
	}
	return pn, nil
}

// podNet is podNetwork for u's container, owned by its ID.
func (u *Unikontainer) podNet() (*network.PodNetwork, error) {
	pn, err := podNetwork(u.Spec.Annotations)
	if pn != nil && u.State != nil {
		pn.Owner = u.State.ID
	}
	return pn, err
}
