package tailgate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func forwardCommand(t *testing.T, args ...string) (*exec.Cmd, string) {
	t.Helper()
	temp := t.TempDir()
	argsFile := filepath.Join(temp, "ssh-args")
	for name, content := range map[string]string{
		"docker": "#!/bin/sh\n[ \"$*\" = \"compose config --environment\" ] && exit 0\necho \"unexpected docker invocation: $*\" >&2\nexit 99\n",
		"ssh":    "#!/bin/sh\nprintf '<%s>\\n' \"$@\" > \"$FAKE_SSH_ARGS\"\n",
	} {
		if err := os.WriteFile(filepath.Join(temp, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("bash", append([]string{"bin/tailgate", "forward"}, args...)...)
	cmd.Env = isolatedTestEnv(
		"PATH="+temp+":"+os.Getenv("PATH"),
		"TAILGATE_ENV_FILE="+filepath.Join(temp, "missing.env"),
		"FAKE_SSH_ARGS="+argsFile,
	)
	return cmd, argsFile
}

func TestForwardBuildsEndpointSpecs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		want     []string
		wantInfo string
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
			name:     "ts expands SSH target hostname",
			args:     []string{"user@myhost.ts.example.com", "ts:8000"},
			want:     []string{"<-N>", "<-o>", "<ExitOnForwardFailure=yes>", "<-o>", "<ProxyCommand=nc -X 5 -x 127.0.0.1:1055 %h %p>", "<-L>", "<127.0.0.1:8000:myhost.ts.example.com:8000>", "<user@myhost.ts.example.com>"},
			wantInfo: "user@myhost.ts.example.com:myhost.ts.example.com:8000",
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, argsFile := forwardCommand(t, tc.args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("forward failed: %v\n%s", err, out)
			}
			got, err := os.ReadFile(argsFile)
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
