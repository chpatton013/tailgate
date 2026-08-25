package tailgate_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var (
	suiteImage       string
	suiteImageReason string
	suiteImageBuilt  bool
)

func TestMain(m *testing.M) {
	flag.Parse()
	suiteImage = os.Getenv("TAILGATE_TEST_IMAGE")
	if suiteImage == "" {
		switch {
		case testing.Short():
			suiteImageReason = "image tests disabled by -short"
		default:
			if _, err := exec.LookPath("docker"); err != nil {
				suiteImageReason = "docker is not installed"
				break
			}
			if out, err := exec.Command("docker", "version").CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "docker is installed but unavailable: %v\n%s", err, out)
				os.Exit(1)
			}
			suiteImage = fmt.Sprintf("tailgate:test-%d", os.Getpid())
			if out, err := exec.Command("docker", "build", "-t", suiteImage, ".").CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "build image tests fixture: %v\n%s", err, out)
				os.Exit(1)
			}
			suiteImageBuilt = true
		}
	}

	code := m.Run()
	if suiteImageBuilt {
		if out, err := exec.Command("docker", "image", "rm", "-f", suiteImage).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "remove image tests fixture: %v\n%s", err, out)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

func testImage(t *testing.T) string {
	t.Helper()
	if suiteImage == "" {
		t.Skip(suiteImageReason)
	}
	return suiteImage
}

func dockerCommand(t *testing.T, timeout time.Duration, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, "docker", args...)
}

func uniqueContainerName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("tailgate-test-%d", time.Now().UnixNano())
}

func runFastFailure(t *testing.T, expectedLog string, environment ...string) {
	t.Helper()
	name := uniqueContainerName(t)
	args := []string{"run", "--rm", "--name", name}
	for _, value := range environment {
		args = append(args, "-e", value)
	}
	args = append(args, testImage(t))

	started := time.Now()
	out, err := dockerCommand(t, 15*time.Second, args...).CombinedOutput()
	if err == nil {
		t.Fatalf("container succeeded after child failure:\n%s", out)
	}
	if time.Since(started) >= 15*time.Second {
		t.Fatalf("container did not stop its sibling promptly:\n%s", out)
	}
	if !strings.Contains(string(out), expectedLog) {
		t.Fatalf("missing %q in child output:\n%s", expectedLog, out)
	}
}

func TestImageExplicitlyRunsSupervisor(t *testing.T) {
	var inspected []struct {
		Config struct {
			Entrypoint []string
			Cmd        []string
		}
	}
	out, err := dockerCommand(t, 10*time.Second, "image", "inspect", testImage(t)).Output()
	if err != nil {
		t.Fatalf("inspect image: %v", err)
	}
	if err := json.Unmarshal(out, &inspected); err != nil {
		t.Fatalf("decode image config: %v", err)
	}
	if len(inspected) != 1 {
		t.Fatalf("inspect returned %d images", len(inspected))
	}
	if got := inspected[0].Config.Entrypoint; len(got) != 1 || got[0] != "/usr/local/bin/tailgate-entrypoint" {
		t.Fatalf("entrypoint = %#v", got)
	}
	if got := inspected[0].Config.Cmd; len(got) != 0 {
		t.Fatalf("inherited command reaches supervisor as arguments: %#v", got)
	}
}

func TestImageContainerbootFailureStopsShim(t *testing.T) {
	runFastFailure(t,
		"containerboot exited with status 1",
		"TS_SOCKS5_SERVER=invalid-listen",
		testAssignment("TAILGATE_TAILNET_SUFFIX", "ts.example.com"),
	)
}

func TestImageShimFailureStopsContainerboot(t *testing.T) {
	runFastFailure(t,
		"tailgate-socks:",
		testAssignment("TS_AUTHKEY", "tskey-auth-test"),
		testAssignment("TS_LOGIN_SERVER", "http://192.0.2.1"),
		"TAILGATE_SOCKS_LISTEN=invalid-listen",
		testAssignment("TAILGATE_TAILNET_SUFFIX", "ts.example.com"),
	)
}

func TestImageSIGTERMStopsBothChildren(t *testing.T) {
	image := testImage(t)
	name := uniqueContainerName(t)
	args := []string{
		"run", "-d", "--name", name,
		"-e", testAssignment("TS_AUTHKEY", "tskey-auth-test"),
		"-e", testAssignment("TS_LOGIN_SERVER", "http://192.0.2.1"),
		"-e", testAssignment("TAILGATE_TAILNET_SUFFIX", "ts.example.com"),
		image,
	}
	out, err := dockerCommand(t, 15*time.Second, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("start container: %v\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})
	time.Sleep(time.Second)

	state, err := dockerCommand(t, 5*time.Second, "inspect", "-f", "{{.State.Running}}", containerID).Output()
	if err != nil || strings.TrimSpace(string(state)) != "true" {
		logs, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
		t.Fatalf("container exited before SIGTERM: %v\n%s", err, logs)
	}

	started := time.Now()
	out, err = dockerCommand(t, 10*time.Second, "stop", "-t", "5", containerID).CombinedOutput()
	if err != nil {
		t.Fatalf("stop container: %v\n%s", err, out)
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("PID 1 did not forward SIGTERM and reap children promptly: %s", elapsed)
	}
	exitCode, err := dockerCommand(t, 5*time.Second, "inspect", "-f", "{{.State.ExitCode}}", containerID).Output()
	if err != nil {
		t.Fatalf("inspect stopped container: %v", err)
	}
	if got := strings.TrimSpace(string(exitCode)); got != "143" {
		logs, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
		t.Fatalf("exit status = %s, want 143 (graceful SIGTERM)\n%s", got, logs)
	}
}
