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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/unikernels"
)

func containersConfig(c UruncContainers) *UruncConfig {
	cfg := defaultUruncConfig()
	cfg.Containers = c
	return cfg
}

func podBootAnnots() map[string]string {
	return map[string]string{
		annotBootKernel: "/pod/vmlinux",
		annotBootInitrd: "/pod/initrd",
		annotHypervisor: string(hypervisors.FirecrackerVmm),
	}
}

func TestNewRequireVM(t *testing.T) {
	requireVM := containersConfig(UruncContainers{RequireVM: true})

	t.Run("container without annotations is refused", func(t *testing.T) {
		t.Parallel()
		_, err := New(writeBundle(t, map[string]string{}), "c", t.TempDir(), requireVM)
		assert.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotUnikernel, "must not fall back to runc")
		assert.ErrorContains(t, err, "require_vm")
	})

	t.Run("queue-proxy container is refused and its config is untouched", func(t *testing.T) {
		t.Parallel()
		bundle := writeBundle(t, map[string]string{annotCRICntrName: criQueueProxyCntr})
		before, err := os.ReadFile(filepath.Join(bundle, configFilename))
		assert.NoError(t, err)

		_, err = New(bundle, "c", t.TempDir(), requireVM)
		assert.Error(t, err)
		assert.NotErrorIs(t, err, ErrQueueProxy, "must not fall back to runc")
		assert.ErrorContains(t, err, "require_vm")

		after, err := os.ReadFile(filepath.Join(bundle, configFilename))
		assert.NoError(t, err)
		assert.Equal(t, before, after)
	})

	for _, key := range []string{annotCRIContainerType, annotCRIOContainerType} {
		t.Run("CRI sandbox still goes to runc ("+key+")", func(t *testing.T) {
			t.Parallel()
			_, err := New(writeBundle(t, map[string]string{key: criSandboxContainer}), "c", t.TempDir(), requireVM)
			assert.ErrorIs(t, err, ErrNotUnikernel)
		})
	}

	t.Run("unikernel container is accepted", func(t *testing.T) {
		t.Parallel()
		_, err := New(writeBundle(t, validAnnots()), "c", t.TempDir(), requireVM)
		assert.NoError(t, err)
	})

	t.Run("boot annotations are accepted", func(t *testing.T) {
		t.Parallel()
		_, err := New(writeBundle(t, podBootAnnots()), "c", t.TempDir(), requireVM)
		assert.NoError(t, err)
	})
}

func TestNewConfiguredBoot(t *testing.T) {
	t.Run("configured boot replaces the runc fallback", func(t *testing.T) {
		t.Parallel()
		cfg := containersConfig(UruncContainers{
			BootKernel:     "/opt/vmlinux",
			BootInitrd:     "/opt/initrd",
			BootHypervisor: string(hypervisors.CloudHypervisorVmm),
		})
		u, err := New(writeBundle(t, map[string]string{}), "c", t.TempDir(), cfg)
		assert.NoError(t, err)
		a := u.State.Annotations
		assert.Equal(t, "/opt/vmlinux", a[annotBootKernel])
		assert.Equal(t, "/opt/initrd", a[annotBootInitrd])
		assert.Equal(t, string(hypervisors.CloudHypervisorVmm), a[annotHypervisor])
		assert.Equal(t, unikernels.LinuxUnikernel, a[annotType])
		assert.Equal(t, "true", a[annotMountRootfs])
	})

	t.Run("configured boot defaults to qemu", func(t *testing.T) {
		t.Parallel()
		cfg := containersConfig(UruncContainers{BootKernel: "/opt/vmlinux", BootInitrd: "/opt/initrd"})
		u, err := New(writeBundle(t, map[string]string{}), "c", t.TempDir(), cfg)
		assert.NoError(t, err)
		assert.Equal(t, string(hypervisors.QemuVmm), u.State.Annotations[annotHypervisor])
	})

	t.Run("half configured boot is an error", func(t *testing.T) {
		t.Parallel()
		cfg := containersConfig(UruncContainers{BootKernel: "/opt/vmlinux"})
		_, err := New(writeBundle(t, map[string]string{}), "c", t.TempDir(), cfg)
		assert.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotUnikernel)
	})

	t.Run("pod boot annotations win without pin_boot", func(t *testing.T) {
		t.Parallel()
		cfg := containersConfig(UruncContainers{BootKernel: "/opt/vmlinux", BootInitrd: "/opt/initrd"})
		u, err := New(writeBundle(t, podBootAnnots()), "c", t.TempDir(), cfg)
		assert.NoError(t, err)
		a := u.State.Annotations
		assert.Equal(t, "/pod/vmlinux", a[annotBootKernel])
		assert.Equal(t, "/pod/initrd", a[annotBootInitrd])
		assert.Equal(t, string(hypervisors.FirecrackerVmm), a[annotHypervisor])
	})

	t.Run("pin_boot overrides the pod boot annotations", func(t *testing.T) {
		t.Parallel()
		cfg := containersConfig(UruncContainers{
			BootKernel:     "/opt/vmlinux",
			BootInitrd:     "/opt/initrd",
			BootHypervisor: string(hypervisors.CloudHypervisorVmm),
			PinBoot:        true,
		})
		u, err := New(writeBundle(t, podBootAnnots()), "c", t.TempDir(), cfg)
		assert.NoError(t, err)
		a := u.State.Annotations
		assert.Equal(t, "/opt/vmlinux", a[annotBootKernel])
		assert.Equal(t, "/opt/initrd", a[annotBootInitrd])
		assert.Equal(t, string(hypervisors.CloudHypervisorVmm), a[annotHypervisor])
	})

	t.Run("pin_boot without a configured boot refuses a pod boot", func(t *testing.T) {
		t.Parallel()
		cfg := containersConfig(UruncContainers{PinBoot: true})
		_, err := New(writeBundle(t, podBootAnnots()), "c", t.TempDir(), cfg)
		assert.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotUnikernel)
		assert.ErrorContains(t, err, "pin_boot")
	})

	t.Run("pin_boot leaves unikernel containers alone", func(t *testing.T) {
		t.Parallel()
		cfg := containersConfig(UruncContainers{
			BootKernel: "/opt/vmlinux",
			BootInitrd: "/opt/initrd",
			PinBoot:    true,
		})
		annots := validAnnots()
		u, err := New(writeBundle(t, annots), "c", t.TempDir(), cfg)
		assert.NoError(t, err)
		for key, val := range annots {
			assert.Equal(t, val, u.State.Annotations[key])
		}
		assert.Empty(t, u.State.Annotations[annotBootKernel])
	})
}

func TestLoadUruncConfigContainers(t *testing.T) {
	t.Run("containers table is decoded", func(t *testing.T) {
		t.Parallel()
		path := writeTestConfig(t, `
[containers]
require_vm = true
boot_kernel = "/opt/vmlinux"
boot_initrd = "/opt/initrd"
boot_hypervisor = "cloud-hypervisor"
pin_boot = true
`)
		config, err := LoadUruncConfig(path)
		assert.NoError(t, err)
		assert.Equal(t, UruncContainers{
			RequireVM:      true,
			BootKernel:     "/opt/vmlinux",
			BootInitrd:     "/opt/initrd",
			BootHypervisor: "cloud-hypervisor",
			PinBoot:        true,
		}, config.Containers)
	})

	t.Run("missing containers table is the zero value", func(t *testing.T) {
		t.Parallel()
		path := writeTestConfig(t, "[runtime]\nvAccel = true\n")
		config, err := LoadUruncConfig(path)
		assert.NoError(t, err)
		assert.Equal(t, UruncContainers{}, config.Containers)
	})
}
