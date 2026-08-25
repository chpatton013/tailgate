package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorHandlesSignalBufferedDuringStartup(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	children := []*child{
		helperChild(t, "first"),
		helperChild(t, "second"),
	}

	started := time.Now()
	if got := supervise(children, signals); got != 143 {
		t.Fatalf("exit code = %d, want 143", got)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("buffered startup signal was not handled promptly: %s", elapsed)
	}
}

func TestSupervisorCleansStartedChildWhenNextStartFails(t *testing.T) {
	children := []*child{
		helperChild(t, "started"),
		{name: "missing", cmd: exec.Command("/definitely/missing/tailgate-child")},
	}

	started := time.Now()
	if got := supervise(children, make(chan os.Signal)); got != 1 {
		t.Fatalf("exit code = %d, want 1", got)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("started child was not cleaned up promptly: %s", elapsed)
	}
}

func helperChild(t *testing.T, name string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestEntrypointHelperProcess", "--")
	cmd.Env = append(os.Environ(), "TAILGATE_ENTRYPOINT_HELPER=1")
	return &child{name: name, cmd: cmd}
}

func TestEntrypointHelperProcess(t *testing.T) {
	if os.Getenv("TAILGATE_ENTRYPOINT_HELPER") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	<-signals
	os.Exit(128 + int(syscall.SIGTERM))
}
