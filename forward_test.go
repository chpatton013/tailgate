package tailgate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type forwardFixture struct {
	cmd             *exec.Cmd
	sshArgsFile     string
	sshConfigFile   string
	resolveArgsFile string
}

func forwardCommand(t *testing.T, args ...string) forwardFixture {
	return forwardCommandWithEnv(t, nil, args...)
}

func forwardCommandWithEnv(t *testing.T, extraEnv []string, args ...string) forwardFixture {
	t.Helper()
	temp := t.TempDir()
	fixture := forwardFixture{
		sshArgsFile:     filepath.Join(temp, "ssh-args"),
		sshConfigFile:   filepath.Join(temp, "ssh-config"),
		resolveArgsFile: filepath.Join(temp, "resolve-args"),
	}
	for name, content := range map[string]string{
		"docker": `#!/bin/sh
case "$*" in
  *"config --environment"*) exit 0 ;;
  *"tailgate-socks --resolve"*)
    printf '<%s>\n' "$@" > "$FAKE_RESOLVE_ARGS"
    [ "${FAKE_DNS_FAIL:-}" != 1 ] || exit 1
    printf '%s\n' "${FAKE_TS_ADDRESS:-100.64.0.8}"
    ;;
  *) echo "unexpected docker invocation: $*" >&2; exit 99 ;;
esac
`,
		"ssh": `#!/bin/sh
if [ "$1" = -G ]; then
  printf '<%s>\n' "$@" > "$FAKE_SSH_CONFIG"
  printf 'hostname %s\n' "${FAKE_SSH_HOSTNAME:-myhost.ts.example.com}"
  exit 0
fi
printf '<%s>\n' "$@" > "$FAKE_SSH_ARGS"
`,
	} {
		if err := os.WriteFile(filepath.Join(temp, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	fixture.cmd = exec.Command("bash", append([]string{"bin/tailgate", "forward"}, args...)...)
	fixture.cmd.Env = append(isolatedTestEnv(
		"PATH="+temp+":"+os.Getenv("PATH"),
		"TAILGATE_ENV_FILE="+filepath.Join(temp, "missing.env"),
		"FAKE_SSH_ARGS="+fixture.sshArgsFile,
		"FAKE_SSH_CONFIG="+fixture.sshConfigFile,
		"FAKE_RESOLVE_ARGS="+fixture.resolveArgsFile,
	), extraEnv...)
	return fixture
}

func TestForwardBuildsEndpointSpecs(t *testing.T) {
	for _, tc := range []struct {
		name            string
		args            []string
		env             []string
		want            []string
		wantInfo        string
		wantResolveArgs []string
	}{
		{
			name:     "legacy loopback defaults",
			args:     []string{"myhost", "8000", "-i", "key"},
			want:     []string{"<-N>", "<-o>", "<ExitOnForwardFailure=yes>", "<-o>", "<ProxyCommand=nc -X 5 -x 127.0.0.1:1055 %h %p>", "<-L>", "<127.0.0.1:8000:127.0.0.1:8000>", "<-i>", "<key>", "<myhost>"},
			wantInfo: "localhost:8000  ->  myhost:127.0.0.1:8000",
		},
		{
			name:     "explicit remote address",
			args:     []string{"myhost", "100.64.0.8:8080", "18080"},
			want:     []string{"<-N>", "<-o>", "<ExitOnForwardFailure=yes>", "<-o>", "<ProxyCommand=nc -X 5 -x 127.0.0.1:1055 %h %p>", "<-L>", "<127.0.0.1:18080:100.64.0.8:8080>", "<myhost>"},
			wantInfo: "localhost:18080  ->  myhost:100.64.0.8:8080",
		},
		{
			name:            "ts resolves a bare SSH alias through Tailgate DNS",
			args:            []string{"kern", "ts:8000", "28789"},
			env:             []string{"FAKE_SSH_HOSTNAME=kern", "FAKE_TS_ADDRESS=100.64.0.9"},
			want:            []string{"<-N>", "<-o>", "<ExitOnForwardFailure=yes>", "<-o>", "<ProxyCommand=nc -X 5 -x 127.0.0.1:1055 %h %p>", "<-L>", "<127.0.0.1:28789:100.64.0.9:8000>", "<kern>"},
			wantInfo:        "kern:100.64.0.9:8000",
			wantResolveArgs: []string{"<exec>", "<-T>", "<tailscale-userspace>", "<tailgate-socks>", "<--resolve>", "<kern.ts.example.com>"},
		},
		{
			name:     "explicit local address and port",
			args:     []string{"myhost", "service.internal:8000", "0.0.0.0:18080"},
			want:     []string{"<-N>", "<-o>", "<ExitOnForwardFailure=yes>", "<-o>", "<ProxyCommand=nc -X 5 -x 127.0.0.1:1055 %h %p>", "<-L>", "<0.0.0.0:18080:service.internal:8000>", "<myhost>"},
			wantInfo: "http://0.0.0.0:18080",
		},
		{
			name:     "IPv6 endpoints",
			args:     []string{"myhost", "[fd7a::12]:8000", "[::1]:18080"},
			want:     []string{"<-N>", "<-o>", "<ExitOnForwardFailure=yes>", "<-o>", "<ProxyCommand=nc -X 5 -x 127.0.0.1:1055 %h %p>", "<-L>", "<[::1]:18080:[fd7a::12]:8000>", "<myhost>"},
			wantInfo: "[fd7a::12]:8000",
		},
		{
			name:     "SSH option with colon is not a local endpoint",
			args:     []string{"myhost", "8000", "-L127.0.0.1:8080:other:80"},
			want:     []string{"<-N>", "<-o>", "<ExitOnForwardFailure=yes>", "<-o>", "<ProxyCommand=nc -X 5 -x 127.0.0.1:1055 %h %p>", "<-L>", "<127.0.0.1:8000:127.0.0.1:8000>", "<-L127.0.0.1:8080:other:80>", "<myhost>"},
			wantInfo: "localhost:8000  ->  myhost:127.0.0.1:8000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := forwardCommandWithEnv(t, tc.env, tc.args...)
			out, err := fixture.cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("forward failed: %v\n%s", err, out)
			}
			got, err := os.ReadFile(fixture.sshArgsFile)
			if err != nil {
				t.Fatal(err)
			}
			gotArgs := strings.Split(strings.TrimSpace(string(got)), "\n")
			if strings.Join(gotArgs, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("ssh arguments = %q, want %q", gotArgs, tc.want)
			}
			if !strings.Contains(string(out), tc.wantInfo) {
				t.Errorf("forward output missing %q:\n%s", tc.wantInfo, out)
			}
			if len(tc.wantResolveArgs) > 0 {
				gotResolveArgs, err := os.ReadFile(fixture.resolveArgsFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(gotResolveArgs) != strings.Join(tc.wantResolveArgs, "\n")+"\n" {
					t.Errorf("resolver arguments = %q, want %q", gotResolveArgs, tc.wantResolveArgs)
				}
			}
		})
	}
}

func TestForwardRejectsMalformedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"non-numeric remote port", []string{"myhost", "service:abc"}, "remote endpoint port must be numeric"},
		{"unbracketed IPv6", []string{"myhost", "fd7a::12:8000"}, "IPv6 addresses must use bracketed syntax"},
		{"missing remote address", []string{"myhost", ":8000"}, "remote endpoint address must not be empty"},
		{"malformed local endpoint", []string{"myhost", "8000", "localhost:abc"}, "local endpoint port must be numeric"},
		{"unexpected local token", []string{"myhost", "8000", "not-an-endpoint"}, "local endpoint 'not-an-endpoint' is malformed"},
		{"unsafe host", []string{"myhost;touch /tmp/pwned", "8000"}, "host contains unsupported shell characters"},
		{"leading-dash host", []string{"-myhost", "8000"}, "host contains unsupported shell characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _ := forwardCommand(t, tc.args...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("forward accepted malformed endpoint:\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("forward output missing %q:\n%s", tc.want, out)
			}
		})
	}
}

func sshCommand(t *testing.T, args ...string) (*exec.Cmd, string) {
	t.Helper()
	temp := t.TempDir()
	sshArgsFile := filepath.Join(temp, "ssh-args")
	ssh := filepath.Join(temp, "ssh")
	if err := os.WriteFile(ssh, []byte(`#!/bin/sh
printf '<%s>\n' "$@" > "$FAKE_SSH_ARGS"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"bin/tailgate", "ssh"}, args...)...)
	cmd.Env = isolatedTestEnv(
		"PATH="+temp+":"+os.Getenv("PATH"),
		"TAILGATE_ENV_FILE="+filepath.Join(temp, "missing.env"),
		"FAKE_SSH_ARGS="+sshArgsFile,
	)
	return cmd, sshArgsFile
}

func TestSSHFindsTargetAfterOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		target string
	}{
		{"separate option argument", []string{"-i", "key", "user@myhost.ts.example.com"}, "user@myhost.ts.example.com"},
		{"attached forwarding option and IPv6 target", []string{"-L127.0.0.1:8080:other:80", "user@[fd7a::12]"}, "user@[fd7a::12]"},
		{"end of options", []string{"--", "user@[fd7a::12]"}, "user@[fd7a::12]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, sshArgsFile := sshCommand(t, tc.args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("ssh failed: %v\n%s", err, out)
			}
			got, err := os.ReadFile(sshArgsFile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), "<"+tc.target+">") {
				t.Errorf("ssh arguments missing target %q: %s", tc.target, got)
			}
		})
	}
}

func TestSSHRejectsUnsafeTargetAfterOptions(t *testing.T) {
	cmd, sshArgsFile := sshCommand(t, "-i", "key", "bad;touch /tmp/pwned")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("ssh accepted unsafe target after option argument:\n%s", out)
	}
	if !strings.Contains(string(out), "host contains unsupported shell characters") {
		t.Errorf("ssh output missing validation error:\n%s", out)
	}
	if _, err := os.Stat(sshArgsFile); err == nil {
		t.Fatal("ssh invoked despite unsafe target")
	}
}

func TestSSHRejectsCombinedOptionCluster(t *testing.T) {
	cmd, sshArgsFile := sshCommand(t, "-vi", "key", "myhost")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("ssh accepted a combined option cluster:\n%s", out)
	}
	if !strings.Contains(string(out), "combined SSH option clusters are unsupported") {
		t.Errorf("ssh output missing cluster validation error:\n%s", out)
	}
	if _, err := os.Stat(sshArgsFile); err == nil {
		t.Fatal("ssh invoked despite unsupported option cluster")
	}
}

func TestSSHRejectsUnsafeTarget(t *testing.T) {
	cmd, sshArgsFile := sshCommand(t, "myhost;touch /tmp/pwned")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("ssh accepted unsafe target:\n%s", out)
	}
	if !strings.Contains(string(out), "host contains unsupported shell characters") {
		t.Errorf("ssh output missing validation error:\n%s", out)
	}
	if _, err := os.Stat(sshArgsFile); err == nil {
		t.Fatal("ssh invoked despite unsafe target")
	}
}
