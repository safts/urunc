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
	"os/exec"
	"testing"
	"time"
)

// stubExecExit replaces execExit for one test and returns the channel that
// receives the code it was called with.
func stubExecExit(t *testing.T) <-chan int {
	t.Helper()
	codes := make(chan int, 1)
	prev := execExit
	execExit = func(code int) { codes <- code }
	t.Cleanup(func() { execExit = prev })
	return codes
}

// TestExecEndsWhenInitExits checks that an exec proxy ends when the
// container's init exits, even if nothing ever reaps the init (it stays a
// zombie, as it does until the shim collects it). The shim does not publish
// the container's exit while any exec of it is still running, so a proxy that
// outlived its VM held the container "running" forever.
func TestExecEndsWhenInitExits(t *testing.T) {
	codes := stubExecExit(t)

	init := exec.Command("sleep", "60")
	if err := init.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = init.Process.Kill(); _ = init.Wait() }()

	exitWithInit(init.Process.Pid)

	select {
	case code := <-codes:
		t.Fatalf("exec ended (code %d) while the init was still running", code)
	case <-time.After(200 * time.Millisecond):
	}

	// Kill it but do not reap it: it stays a zombie.
	if err := init.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-codes:
		if code != execInitGoneCode {
			t.Fatalf("exec ended with code %d, want %d", code, execInitGoneCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec still running 5s after the container's init exited")
	}
}

// TestExecEndsWhenInitAlreadyGone checks that an exec started after the init
// is gone ends at once.
func TestExecEndsWhenInitAlreadyGone(t *testing.T) {
	codes := stubExecExit(t)

	init := exec.Command("true")
	if err := init.Run(); err != nil {
		t.Fatal(err)
	}

	exitWithInit(init.Process.Pid)

	select {
	case code := <-codes:
		if code != execInitGoneCode {
			t.Fatalf("exec ended with code %d, want %d", code, execInitGoneCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec still running although the container's init was already gone")
	}
}
