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

package containerdshim

import (
	"context"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	containerdShim "github.com/urunc-dev/urunc/pkg/containerd-shim/containerd"
	"google.golang.org/protobuf/types/known/emptypb"
)

// taskService is urunc's shim-side wrapper around containerd's runc task
// service. It wires urunc task setup before forwarding calls to the wrapped
// service.
type taskService struct {
	taskAPI.TaskService

	containerdAddress string
}

func (s *taskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	session, err := containerdShim.OpenSession(ctx, s.containerdAddress, r.ID)
	if err != nil {
		log.G(ctx).WithError(err).Warn("urunc(shim): failed to open containerd session")
	} else {
		defer func() {
			if err := session.Close(); err != nil {
				log.G(ctx).WithError(err).Warn("urunc(shim): failed to close containerd session")
			}
		}()
		if err := containerdShim.InjectUruncAnnotations(ctx, session, r.Bundle); err != nil {
			log.G(ctx).WithError(err).Warn("urunc(shim): failed to inject annotations to spec")
		}
	}

	resp, err := s.TaskService.Create(ctx, r)
	if err != nil {
		return resp, err
	}

	// TODO: #816 - Restore rootfs choice here once shim integration is complete.
	// For now, rootfs is selected during urunc create (InitialSetup phase).
	// ChooseRootfs after inner task Create so bundle rootfs is mounted;
	// params are persisted in bundle config.json for runtime Exec.
	// if err := chooseGuestRootfs(r); err != nil {
	// 	if errors.Is(err, errGuestRootfsChoiceSkipped) {
	// 		log.G(ctx).WithError(err).Debug("urunc(shim): guest rootfs choice skipped")
	// 		return resp, nil
	// 	}
	// 	log.G(ctx).WithError(err).Warn("urunc(shim): failed to choose guest rootfs")
	// 	return nil, err
	// }

	return resp, nil
}

func (s *taskService) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	return s.TaskService.Delete(ctx, r)
}

// Kill logs each request and its outcome, so a stop that never completes shows
// how far it got.
func (s *taskService) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
	l := log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID, "signal": r.Signal, "all": r.All})
	l.Info("urunc(shim): kill")
	resp, err := s.TaskService.Kill(ctx, r)
	if err != nil {
		l.WithError(err).Info("urunc(shim): kill failed")
	}
	return resp, err
}

// exitLogger logs every task exit the shim publishes. The shim holds a
// container's exit back until all its execs have exited, so a stop waiting
// forever with no "publishing task exit" line points there.
type exitLogger struct {
	shim.Publisher
}

func (p exitLogger) Publish(ctx context.Context, topic string, event events.Event) error {
	if e, ok := event.(*eventstypes.TaskExit); ok {
		log.G(ctx).WithFields(log.Fields{"id": e.ContainerID, "exec": e.ID, "pid": e.Pid, "status": e.ExitStatus}).
			Info("urunc(shim): publishing task exit")
	}
	return p.Publisher.Publish(ctx, topic, event)
}

func (s *taskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, s)
	return nil
}
