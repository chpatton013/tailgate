package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const shutdownTimeout = 5 * time.Second

type child struct {
	name string
	cmd  *exec.Cmd
}

type childResult struct {
	child *child
	err   error
}

func main() {
	os.Exit(run())
}

func run() int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	children := []*child{
		newChild("tailgate-socks", "/usr/local/bin/tailgate-socks"),
		newChild("containerboot", "/usr/local/bin/containerboot"),
	}
	return supervise(children, signals)
}

func supervise(children []*child, signals <-chan os.Signal) int {
	results := make(chan childResult, len(children))
	started := make([]*child, 0, len(children))
	for _, process := range children {
		if err := process.cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "tailgate-entrypoint: start %s: %v\n", process.name, err)
			stopAndWait(started, results, syscall.SIGTERM)
			return 1
		}
		started = append(started, process)
		go func(process *child) {
			results <- childResult{child: process, err: process.cmd.Wait()}
		}(process)
	}

	select {
	case result := <-results:
		status := exitCode(result.err)
		fmt.Fprintf(os.Stderr, "tailgate-entrypoint: %s exited with status %d\n", result.child.name, status)
		remaining := childrenExcept(started, result.child)
		stopAndWait(remaining, results, syscall.SIGTERM)
		return status
	case received := <-signals:
		sig := received.(syscall.Signal)
		stopAndWait(started, results, sig)
		return 128 + int(sig)
	}
}

func newChild(name, path string) *child {
	cmd := exec.Command(path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return &child{name: name, cmd: cmd}
}

func childrenExcept(children []*child, excluded *child) []*child {
	remaining := make([]*child, 0, len(children)-1)
	for _, child := range children {
		if child != excluded {
			remaining = append(remaining, child)
		}
	}
	return remaining
}

func stopAndWait(children []*child, results <-chan childResult, sig syscall.Signal) {
	for _, child := range children {
		if child.cmd.Process != nil {
			_ = child.cmd.Process.Signal(sig)
		}
	}

	waiting := len(children)
	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	for waiting > 0 {
		select {
		case <-results:
			waiting--
		case <-timer.C:
			for _, child := range children {
				if child.cmd.Process != nil {
					_ = child.cmd.Process.Kill()
				}
			}
			for waiting > 0 {
				<-results
				waiting--
			}
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return 1
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok {
		return 1
	}
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}
