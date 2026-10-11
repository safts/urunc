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

//go:build linux

package main

import (
	"encoding/json"
	"syscall"
	"testing"
	"time"

	"github.com/urunc-dev/urunc/pkg/agentproto"
)

// recordSignalInit replaces signalInit for one test so no real PID 1 is
// signalled; it returns a channel of the signals the agent asked for.
func recordSignalInit(t *testing.T, ret error) chan syscall.Signal {
	t.Helper()
	got := make(chan syscall.Signal, 4)
	orig := signalInit
	signalInit = func(s syscall.Signal) error {
		got <- s
		return ret
	}
	t.Cleanup(func() { signalInit = orig })
	return got
}

func TestSignalInitStreamDeliversToPID1(t *testing.T) {
	got := recordSignalInit(t, nil)
	cli := dialAgent(t)
	if err := agentproto.WriteJSON(cli, agentproto.TypeSignal, agentproto.InitStream, agentproto.Signal{Signal: 15}); err != nil {
		t.Fatalf("send signal: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := agentproto.ReadFrame(cli)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if f.Type != agentproto.TypeExit || f.Stream != agentproto.InitStream {
		t.Fatalf("reply = type %d stream %d, want Exit on stream 0", f.Type, f.Stream)
	}
	var ex agentproto.Exit
	if err := json.Unmarshal(f.Payload, &ex); err != nil || ex.Code != 0 {
		t.Fatalf("exit payload %q (err %v), want code 0", f.Payload, err)
	}
	select {
	case s := <-got:
		if s != syscall.SIGTERM {
			t.Fatalf("signalInit(%v), want SIGTERM", s)
		}
	default:
		t.Fatal("signalInit was not called")
	}
}

func TestSignalInitStreamReportsFailure(t *testing.T) {
	recordSignalInit(t, syscall.EPERM)
	cli := dialAgent(t)
	if err := agentproto.WriteJSON(cli, agentproto.TypeSignal, agentproto.InitStream, agentproto.Signal{Signal: 15}); err != nil {
		t.Fatalf("send signal: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := agentproto.ReadFrame(cli)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if f.Type != agentproto.TypeError || f.Stream != agentproto.InitStream {
		t.Fatalf("reply = type %d stream %d, want Error on stream 0", f.Type, f.Stream)
	}
}

func TestSignalOnSessionStreamDoesNotSignalInit(t *testing.T) {
	got := recordSignalInit(t, nil)
	cli := dialAgent(t)
	if err := agentproto.WriteJSON(cli, agentproto.TypeSignal, 7, agentproto.Signal{Signal: 15}); err != nil {
		t.Fatalf("send signal: %v", err)
	}
	// A session that runs to completion afterwards proves the frame was handled.
	if _, _, code := runSession(t, cli, 9, agentproto.OpenRequest{Argv: []string{"true"}}); code != 0 {
		t.Fatalf("follow-up session exit %d", code)
	}
	select {
	case s := <-got:
		t.Fatalf("signalInit(%v) called for a session stream", s)
	default:
	}
}
