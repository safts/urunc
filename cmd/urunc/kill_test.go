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
	"testing"
	"time"

	"github.com/urunc-dev/urunc/pkg/agentproto"
	"golang.org/x/sys/unix"
)

// fakeAgent reads the one signal frame signalGuestInit sends, checks it and
// answers with reply (nil: no answer).
func fakeAgent(t *testing.T, agent net.Conn, reply func(net.Conn)) {
	t.Helper()
	go func() {
		f, err := agentproto.ReadFrame(agent)
		if err != nil {
			t.Errorf("fake agent read: %v", err)
			return
		}
		var sg agentproto.Signal
		if f.Type != agentproto.TypeSignal || f.Stream != agentproto.InitStream ||
			json.Unmarshal(f.Payload, &sg) != nil || sg.Signal != 15 {
			t.Errorf("frame = type %d stream %d payload %q, want Signal{15} on stream 0", f.Type, f.Stream, f.Payload)
		}
		if reply != nil {
			reply(agent)
		}
	}()
}

func TestSignalGuestInitSuccess(t *testing.T) {
	host, agent := net.Pipe()
	defer host.Close()
	defer agent.Close()
	fakeAgent(t, agent, func(c net.Conn) {
		_ = agentproto.WriteJSON(c, agentproto.TypeExit, agentproto.InitStream, agentproto.Exit{Code: 0})
	})
	if err := signalGuestInit(host, unix.SIGTERM, 2*time.Second); err != nil {
		t.Fatalf("signalGuestInit: %v", err)
	}
}

func TestSignalGuestInitSkipsOtherStreams(t *testing.T) {
	host, agent := net.Pipe()
	defer host.Close()
	defer agent.Close()
	fakeAgent(t, agent, func(c net.Conn) {
		_ = agentproto.WriteJSON(c, agentproto.TypeError, 3, agentproto.Error{Message: "other"})
		_ = agentproto.WriteJSON(c, agentproto.TypeExit, agentproto.InitStream, agentproto.Exit{Code: 0})
	})
	if err := signalGuestInit(host, unix.SIGTERM, 2*time.Second); err != nil {
		t.Fatalf("signalGuestInit: %v", err)
	}
}

func TestSignalGuestInitError(t *testing.T) {
	host, agent := net.Pipe()
	defer host.Close()
	defer agent.Close()
	fakeAgent(t, agent, func(c net.Conn) {
		_ = agentproto.WriteJSON(c, agentproto.TypeError, agentproto.InitStream, agentproto.Error{Message: "operation not permitted"})
	})
	if err := signalGuestInit(host, unix.SIGTERM, 2*time.Second); err == nil {
		t.Fatal("signalGuestInit succeeded on an Error reply")
	}
}

func TestSignalGuestInitTimeout(t *testing.T) {
	host, agent := net.Pipe()
	defer host.Close()
	defer agent.Close()
	fakeAgent(t, agent, nil)
	if err := signalGuestInit(host, unix.SIGTERM, 100*time.Millisecond); err == nil {
		t.Fatal("signalGuestInit succeeded with no reply")
	}
}
