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
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"

	"github.com/urunc-dev/urunc/pkg/agentproto"
)

// recvConsoleFd accepts one connection on the console socket and returns the
// pty master the proxy hands over, as containerd receives it.
func recvConsoleFd(t *testing.T, l *net.UnixListener) *os.File {
	t.Helper()
	conn, err := l.AcceptUnix()
	if err != nil {
		t.Fatalf("accept console socket: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 256)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatalf("read console fd: %v", err)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parse console fd message: %v (%d messages)", err, len(msgs))
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		t.Fatalf("parse console fd rights: %v", err)
	}
	return os.NewFile(uintptr(fds[0]), "console-master")
}

// TestTTYExecHangsUpGuestWhenConsoleCloses: when the caller closes the pty
// master it got over the console socket (containerd does when the exec's
// client goes away), the proxy must hang up the guest process (SIGHUP) and
// kill it if it is still there, as runc's exec gets SIGHUP from its pty.
func TestTTYExecHangsUpGuestWhenConsoleCloses(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatalf("listen console socket: %v", err)
	}
	defer l.Close()

	cmd := &cli.Command{Flags: []cli.Flag{&cli.StringFlag{Name: "console-socket", Value: sock}}}
	host, agent := net.Pipe()
	defer agent.Close()
	go func() {
		defer host.Close()
		_ = runTTYSession(cmd, host, agentproto.OpenRequest{Argv: []string{"sh"}, TTY: true}, &execReady{})
	}()

	master := recvConsoleFd(t, l)
	f, err := agentproto.ReadFrame(agent)
	if err != nil || f.Type != agentproto.TypeOpen {
		t.Fatalf("first frame = %+v, %v; want Open", f, err)
	}
	_ = master.Close()

	want := []int{int(unix.SIGHUP), int(unix.SIGKILL)}
	_ = agent.SetReadDeadline(time.Now().Add(ttyHangupGrace + 3*time.Second))
	for len(want) > 0 {
		f, err := agentproto.ReadFrame(agent)
		if err != nil {
			t.Fatalf("waiting for signal %d after the console closed: %v", want[0], err)
		}
		if f.Type != agentproto.TypeSignal {
			continue
		}
		var sg agentproto.Signal
		if err := json.Unmarshal(f.Payload, &sg); err != nil || sg.Signal != want[0] {
			t.Fatalf("signal frame %q, want signal %d", f.Payload, want[0])
		}
		want = want[1:]
	}
}
