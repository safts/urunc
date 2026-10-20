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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"

	"golang.org/x/sys/unix"

	"github.com/urunc-dev/urunc/pkg/agentproto"
	"github.com/urunc-dev/urunc/pkg/unikontainers"
)

var killCommand = &cli.Command{
	Name:  "kill",
	Usage: "kill sends the specified signal (default: SIGTERM) to the container's init process",
	ArgsUsage: `<container-id> [signal]

Where "<container-id>" is the name for the instance of the container and
"[signal]" is the signal to be sent to the init process.

EXAMPLE:
For example, if the container id is "ubuntu01" the following will send a "KILL"
signal to the init process of the "ubuntu01" container:

	# urunc kill ubuntu01 KILL`,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "all",
			Aliases: []string{"a"},
			Usage:   "send the specified signal to all processes inside the container",
		},
	},
	Action: func(_ context.Context, cmd *cli.Command) error {
		runtime.GOMAXPROCS(1)
		runtime.LockOSThread()
		logrus.WithField("command", "KILL").WithField("args", os.Args).Debug("urunc INVOKED")
		if err := checkArgs(cmd, 1, minArgs); err != nil {
			return err
		}
		if err := checkArgs(cmd, 2, maxArgs); err != nil {
			return err
		}

		// get Unikontainer data from state.json
		unikontainer, err := getUnikontainer(cmd)
		if err != nil {
			return err
		}

		sigstr := cmd.Args().Get(1)
		if sigstr == "" {
			sigstr = "SIGTERM"
		}

		signal, err := parseSignal(sigstr)
		if err != nil {
			return err
		}

		// Give the guest's init a chance at a graceful stop; on any failure
		// fall back to signalling the monitor, which ends the VM.
		if (signal == unix.SIGTERM || signal == unix.SIGINT) && unikontainer.HasAgent() {
			err := signalAgentInit(unikontainer.AgentTransportInfo(), signal)
			if err == nil {
				logrus.Infof("kill %s: delivered %v to the guest's init", unikontainer.State.ID, signal)
				return nil
			}
			logrus.WithError(err).Warnf("could not deliver %v to the guest's init, signalling the monitor", signal)
		}

		return unikontainer.Signal(signal)
	},
}

// guestSignalTimeout bounds both the agent dial and the wait for its reply.
const guestSignalTimeout = 5 * time.Second

func signalAgentInit(t unikontainers.AgentTransport, sig unix.Signal) error {
	conn, err := dialAgentTransport(t, guestSignalTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	return signalGuestInit(conn, sig, guestSignalTimeout)
}

// signalGuestInit asks the agent on conn to send sig to the guest's PID 1 and
// waits up to timeout for its reply. The read runs in a goroutine because an
// AF_VSOCK connection is an os.File that may not support deadlines; the caller
// closes conn, which ends it.
func signalGuestInit(conn io.ReadWriter, sig unix.Signal, timeout time.Duration) error {
	if err := agentproto.WriteJSON(conn, agentproto.TypeSignal, agentproto.InitStream, agentproto.Signal{Signal: int(sig)}); err != nil {
		return fmt.Errorf("send signal to the in-guest agent: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			f, err := agentproto.ReadFrame(conn)
			if err != nil {
				done <- fmt.Errorf("read the in-guest agent's reply: %w", err)
				return
			}
			if f.Stream != agentproto.InitStream {
				continue
			}
			switch f.Type {
			case agentproto.TypeExit:
				done <- nil
				return
			case agentproto.TypeError:
				var e agentproto.Error
				_ = json.Unmarshal(f.Payload, &e)
				done <- fmt.Errorf("in-guest agent could not signal init: %s", e.Message)
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("no reply from the in-guest agent within %v", timeout)
	}
}

// Taken from https://github.com/opencontainers/runc/blob/af6b9e7fa96026c4fdac93e6e4fddf69ae348532/kill.go
func parseSignal(rawSignal string) (unix.Signal, error) {
	s, err := strconv.Atoi(rawSignal)
	if err == nil {
		return unix.Signal(s), nil
	}
	sig := strings.ToUpper(rawSignal)
	if !strings.HasPrefix(sig, "SIG") {
		sig = "SIG" + sig
	}
	signal := unix.SignalNum(sig)
	if signal == 0 {
		return -1, fmt.Errorf("unknown signal %q", rawSignal)
	}
	return signal, nil
}
