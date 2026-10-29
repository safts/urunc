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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/urunc-dev/urunc/pkg/network"
)

func TestPodNetworkFromAnnotations(t *testing.T) {
	services := "svc-postgres:5432,svc-redis:6379:6380,svc-worker"
	for _, tc := range []struct {
		container string
		want      *network.PodNetwork
	}{
		{"agent", &network.PodNetwork{}},
		{"init", &network.PodNetwork{}},
		{"", &network.PodNetwork{}},
		{"svc-postgres", &network.PodNetwork{Service: 1, Ports: []uint16{5432}}},
		{"svc-redis", &network.PodNetwork{Service: 2, Ports: []uint16{6379, 6380}}},
		{"svc-worker", &network.PodNetwork{Service: 3}},
	} {
		got, err := podNetwork(map[string]string{annotPodServices: services, annotCRICntrName: tc.container})
		require.NoError(t, err, tc.container)
		assert.Equal(t, tc.want, got, tc.container)
	}

	got, err := podNetwork(map[string]string{annotCRICntrName: "agent"})
	require.NoError(t, err)
	assert.Nil(t, got, "without the annotation the pod has no pod network")
}

func TestPodNetworkRejectsBadAnnotations(t *testing.T) {
	for _, v := range []string{
		"svc-a:5432,svc-a:5433",        // a name twice
		"svc-a:5432,svc-b:5432",        // a port twice
		":5432",                        // no name
		"svc-a:0",                      // port 0
		"svc-a:65536",                  // port out of range
		"svc-a:http",                   // not a number
		"svc-a:5432,,svc-b:5433",       // an empty entry
		strings.Repeat("s,", 64) + "s", // too many
	} {
		_, err := podNetwork(map[string]string{annotPodServices: v, annotCRICntrName: "agent"})
		assert.Error(t, err, v)
	}
}

func TestWriteMonitorSpecCarriesThePodNetwork(t *testing.T) {
	monRootfs := t.TempDir()
	u, rootfsParams := newSpecUnikontainer(t, monRootfs)
	u.Spec.Annotations[annotPodServices] = "svc-db:5432"
	u.Spec.Annotations[annotCRICntrName] = "svc-db"

	require.NoError(t, u.writeMonitorSpec(rootfsParams, monitorResources{}))
	got := readMonitorSpecFile(t, monRootfs)
	assert.Equal(t, &network.PodNetwork{Service: 1, Ports: []uint16{5432}}, got.PodNet)

	u.Spec.Annotations[annotPodServices] = "svc-db:5432,svc-db:5433"
	assert.Error(t, u.writeMonitorSpec(rootfsParams, monitorResources{}))
}
