package tailgate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type doctorFixture struct {
	cmd      *exec.Cmd
	argsFile string
}

func doctorCommand(t *testing.T, config []string, failureEnv ...string) doctorFixture {
	t.Helper()
	temp := t.TempDir()
	envFile := filepath.Join(temp, "tailgate.env")
	if err := os.WriteFile(envFile, []byte("# parsed by Compose, never sourced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(temp, "docker-args")

	fakeDocker := `#!/bin/sh
case "$*" in
  *"config --environment"*) printf '%s\n' "$FAKE_COMPOSE_ENV" ;;
  "ps --format {{.Names}}") echo tailgate ;;
  *"pidof tailgate-socks"*) [ "${FAKE_PROCESS_FAIL:-}" != 1 ] ;;
  *"tailscale status --json"*)
    printf '{"BackendState":"%s"}\n' "${FAKE_BACKEND_STATE:-Running}"
    ;;
  *"tailscale ip"*) echo 100.64.0.8 ;;
  *"nc -z 127.0.0.1 1057"*) [ "${FAKE_BACKEND_FAIL:-}" != 1 ] ;;
  *"tailgate-socks --resolve"*)
    printf '<%s>\n' "$@" > "$FAKE_ARGS_FILE"
    [ "${FAKE_DNS_FAIL:-}" != 1 ]
    ;;
  *) echo "unexpected docker invocation: $*" >&2; exit 99 ;;
esac
`
	fakeNC := `#!/bin/sh
[ "${FAKE_FRONT_FAIL:-}" != 1 ]
`
	for name, content := range map[string]string{"docker": fakeDocker, "nc": fakeNC} {
		path := filepath.Join(temp, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("bash", "bin/tailgate", "doctor")
	cmd.Env = isolatedTestEnv(
		"PATH="+temp+":"+os.Getenv("PATH"),
		"TAILGATE_ENV_FILE="+envFile,
		"FAKE_COMPOSE_ENV="+strings.Join(config, "\n"),
		"FAKE_ARGS_FILE="+argsFile,
	)
	cmd.Env = append(cmd.Env, failureEnv...)
	return doctorFixture{cmd: cmd, argsFile: argsFile}
}

func fullDoctorConfig() []string {
	return []string{
		testAssignment("TS_AUTHKEY", "placeholder-auth-key"),
		testAssignment("TS_LOGIN_SERVER", "https://headscale.example.com"),
		"TS_MODE=userspace",
		"TAILGATE_DNS_TEST_NAME=service.ts.example.com",
		"TS_DEBUG_ALWAYS_USE_DERP=true",
	}
}

func TestDoctorDistinguishesProxyLayersAndInternalDNS(t *testing.T) {
	fixture := doctorCommand(t, fullDoctorConfig())
	out, err := fixture.cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"container 'tailgate' running",
		"tailgate processes running",
		"node registered to https://headscale.example.com",
		"DNS-aware SOCKS shim reachable on 127.0.0.1:1055",
		"tailscaled SOCKS backend reachable on 127.0.0.1:1057",
		"internal alias resolves: service.ts.example.com",
		"forced DERP enabled",
		"doctor: all good",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "placeholder-auth-key") {
		t.Fatalf("doctor printed auth key:\n%s", out)
	}
}

func TestDoctorRequiresRunningBackendState(t *testing.T) {
	fixture := doctorCommand(t, fullDoctorConfig(), "FAKE_BACKEND_STATE=NeedsLogin")
	out, err := fixture.cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("doctor accepted NeedsLogin:\n%s", out)
	}
	if !strings.Contains(string(out), "tailscale not registered yet") {
		t.Fatalf("doctor did not report registration failure:\n%s", out)
	}
}

func TestDoctorSkipsOptionalAliasWhenUnset(t *testing.T) {
	config := fullDoctorConfig()
	config = config[:3]
	fixture := doctorCommand(t, config)
	out, err := fixture.cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "internal alias check skipped") {
		t.Fatalf("doctor did not report skipped alias:\n%s", out)
	}
	if _, err := os.Stat(fixture.argsFile); !os.IsNotExist(err) {
		t.Fatalf("doctor invoked alias resolver with no configured alias")
	}
}

func TestDoctorPassesAliasAsOneShellSafeArgument(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "injected")
	alias := "service.ts.example.com;touch " + marker
	config := fullDoctorConfig()
	config[3] = "TAILGATE_DNS_TEST_NAME=" + alias
	fixture := doctorCommand(t, config)
	if out, err := fixture.cmd.CombinedOutput(); err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("alias content executed as shell code")
	}
	args, err := os.ReadFile(fixture.argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "<"+alias+">") {
		t.Fatalf("alias was not passed as one argument:\n%s", args)
	}
}

func TestProxyUsesConfiguredTailnetSuffixWithoutPrintingAuthKey(t *testing.T) {
	temp := t.TempDir()
	envFile := filepath.Join(temp, "tailgate.env")
	if err := os.WriteFile(envFile, []byte(testAssignment("TS_AUTHKEY", "ignored-by-fake")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeDocker := filepath.Join(temp, "docker")
	fakeOutput := strings.Join([]string{
		testAssignment("TS_AUTHKEY", "placeholder-auth-key"),
		testAssignment("TAILGATE_TAILNET_SUFFIX", "corp.ts.example.com"),
	}, "' '")
	fakeScript := "#!/bin/sh\nprintf '%s\\n' '" + fakeOutput + "'\n"
	if err := os.WriteFile(fakeDocker, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "bin/tailgate", "proxy")
	cmd.Env = isolatedTestEnv("PATH="+temp+":"+os.Getenv("PATH"), "TAILGATE_ENV_FILE="+envFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy failed: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{"Host *.corp.ts.example.com 100.64.*", "unset HTTPS_PROXY https_proxy", "export ALL_PROXY=socks5h://127.0.0.1:1055"} {
		if !strings.Contains(text, want) {
			t.Errorf("proxy output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(text, "export HTTPS_PROXY") || strings.Contains(text, "placeholder-auth-key") {
		t.Fatalf("proxy printed unsafe HTTPS proxy or auth key:\n%s", out)
	}
}

func TestProxyUsesDefaultsWhenDockerAndEnvFileAreAbsent(t *testing.T) {
	cmd := exec.Command("bash", "bin/tailgate", "proxy")
	cmd.Env = isolatedTestEnv(
		"PATH=/usr/bin:/bin",
		"TAILGATE_ENV_FILE="+filepath.Join(t.TempDir(), "missing.env"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy failed without Docker or env file: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Host *.ts.example.com 100.64.*") {
		t.Fatalf("proxy did not use safe defaults:\n%s", out)
	}
}

func TestConfigurationIsNotExecutedAndEnvironmentWins(t *testing.T) {
	temp := t.TempDir()
	marker := filepath.Join(temp, "executed")
	envFile := filepath.Join(temp, "tailgate.env")
	content := strings.Join([]string{
		testAssignment("TS_AUTHKEY", "placeholder-auth-key"),
		testAssignment("TAILGATE_TAILNET_SUFFIX", "file.ts.example.com"),
		"UNUSED=$(touch " + marker + ")",
		testAssignment("TS_LOGIN_SERVER", `"https://headscale.example.com/path with space"`),
		"",
	}, "\n")
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "bin/tailgate", "proxy")
	cmd.Env = isolatedTestEnv(
		"TAILGATE_ENV_FILE="+envFile,
		testAssignment("TAILGATE_TAILNET_SUFFIX", "environment.ts.example.com"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal(".env content executed as shell code")
	}
	if !strings.Contains(string(out), "Host *.environment.ts.example.com 100.64.*") {
		t.Fatalf("environment did not override env file:\n%s", out)
	}
}

func TestDoctorReportsLayerFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want string
	}{
		{"process", "FAKE_PROCESS_FAIL=1", "tailgate processes are not healthy"},
		{"front shim", "FAKE_FRONT_FAIL=1", "DNS-aware SOCKS shim not reachable"},
		{"backend", "FAKE_BACKEND_FAIL=1", "tailscaled SOCKS backend not reachable"},
		{"internal DNS", "FAKE_DNS_FAIL=1", "internal alias does not resolve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := doctorCommand(t, fullDoctorConfig(), tc.env)
			out, err := fixture.cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("doctor succeeded despite %s failure:\n%s", tc.name, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("doctor output missing %q:\n%s", tc.want, out)
			}
		})
	}
}
