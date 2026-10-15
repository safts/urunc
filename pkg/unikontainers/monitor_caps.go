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

	"github.com/opencontainers/runc/libcontainer/capabilities"
	"github.com/opencontainers/runc/libcontainer/configs"
)

// dropToExecCaps cuts the bounding set back to execCaps' before the drop to
// the container user, while "urunc init" still has CAP_SETPCAP
// (monSetupCaps). Nothing the monitor execs can then regain a capability of
// the setup's. A nil execCaps (the container declares no capabilities) keeps
// the set the monitor was started with.
func dropToExecCaps(execCaps *configs.Capabilities) error {
	if execCaps == nil {
		return nil
	}
	c, err := capabilities.New(execCaps)
	if err != nil {
		return fmt.Errorf("could not load the monitor's capabilities: %w", err)
	}
	if err = c.ApplyBoundingSet(); err != nil {
		return fmt.Errorf("could not drop the setup's capabilities from the bounding set: %w", err)
	}
	return nil
}

// restoreExecCaps sets the effective, permitted, inheritable and ambient sets
// to execCaps' after the drop to the container user. Only a root monitor
// needs it: a setuid to any other uid has cleared them already.
func restoreExecCaps(execCaps *configs.Capabilities, uid uint32) error {
	if execCaps == nil || uid != 0 {
		return nil
	}
	c, err := capabilities.New(execCaps)
	if err != nil {
		return fmt.Errorf("could not load the monitor's capabilities: %w", err)
	}
	if err = c.ApplyCaps(); err != nil {
		return fmt.Errorf("could not drop the setup's capabilities: %w", err)
	}
	return nil
}
