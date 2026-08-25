package tailgate_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testAssignment(key, value string) string {
	return key + "=" + value
}

func isolatedTestEnv(overrides ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if strings.HasPrefix(key, "TS_") ||
			strings.HasPrefix(key, "TAILGATE_") ||
			strings.HasPrefix(key, "HEADSCALE_") ||
			key == "GO_IMAGE" || key == "TAILSCALE_IMAGE" || key == "COMPOSE_PROFILES" {
			continue
		}
		env = append(env, assignment)
	}
	return append(env, overrides...)
}

func composeConfig(t *testing.T, profile string) map[string]any {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "--env-file", ".env.example", "--profile", profile, "config", "--format", "json")
	cmd.Dir = root
	cmd.Env = isolatedTestEnv(
		testAssignment("TS_AUTHKEY", "test-auth-key"),
		"TS_MODE="+profile,
		"TS_IMAGE_TAG=test-tag",
		"GO_IMAGE=registry.example/go@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"TAILSCALE_IMAGE=registry.example/tailscale@sha256:2222222222222222222222222222222222222222222222222222222222222222",
		testAssignment("TAILGATE_TAILNET_SUFFIX", "ts.test.example"),
		"TS_DEBUG_ALWAYS_USE_DERP=true",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("decode compose config: %v", err)
	}
	return config
}

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want object", value)
	}
	return result
}

func array(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("got %T, want array", value)
	}
	return result
}

func TestComposeProfilesPreserveProxyBoundaries(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		profile string
		service string
	}{
		{"userspace", "tailscale-userspace"},
		{"kernel", "tailscale-kernel"},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			config := composeConfig(t, tc.profile)
			services := object(t, config["services"])
			if len(services) != 1 {
				t.Fatalf("active profile rendered services %#v, want only %q", services, tc.service)
			}
			service := object(t, services[tc.service])
			build := object(t, service["build"])
			if build["context"] != root || build["dockerfile"] != "Dockerfile" {
				t.Fatalf("unexpected build: %#v", build)
			}
			args := object(t, build["args"])
			for name, want := range map[string]any{
				"GO_IMAGE":        "registry.example/go@sha256:1111111111111111111111111111111111111111111111111111111111111111",
				"TAILSCALE_IMAGE": "registry.example/tailscale@sha256:2222222222222222222222222222222222222222222222222222222222222222",
			} {
				if args[name] != want {
					t.Fatalf("build arg %s = %v, want %v", name, args[name], want)
				}
			}
			if service["network_mode"] != "bridge" {
				t.Fatalf("network_mode = %v", service["network_mode"])
			}
			env := object(t, service["environment"])
			for name, want := range map[string]any{
				"TS_SOCKS5_SERVER":            "127.0.0.1:1057",
				"TAILGATE_SOCKS_LISTEN":       "0.0.0.0:1055",
				"TAILGATE_BACKEND_SOCKS_ADDR": "127.0.0.1:1057",
				"TAILGATE_TAILNET_SUFFIX":     "ts.test.example",
				"TS_DEBUG_ALWAYS_USE_DERP":    "true",
			} {
				if env[name] != want {
					t.Errorf("%s = %v, want %v", name, env[name], want)
				}
			}

			ports := array(t, service["ports"])
			if len(ports) != 2 {
				t.Fatalf("published ports: %#v", ports)
			}
			seen := map[string]bool{}
			for _, raw := range ports {
				port := object(t, raw)
				key := fmt.Sprintf("%v:%v:%v", port["host_ip"], port["published"], port["target"])
				seen[key] = true
			}
			for _, want := range []string{"127.0.0.1:1055:1055", "127.0.0.1:1056:1056"} {
				if !seen[want] {
					t.Errorf("missing publication %s in %#v", want, seen)
				}
			}

			volumes := array(t, service["volumes"])
			targets := map[string]bool{}
			for _, raw := range volumes {
				targets[object(t, raw)["target"].(string)] = true
			}
			if !targets["/var/lib/tailscale"] || !targets["/certs"] {
				t.Fatalf("volume targets: %#v", targets)
			}
		})
	}
}

func TestExampleEnvironmentDocumentsDNSShimAndOptionalDERP(t *testing.T) {
	content, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, required := range []string{
		testAssignment("TAILGATE_TAILNET_SUFFIX", "ts.example.com"),
		"TAILGATE_DNS_TEST_NAME=service.ts.example.com",
		"TS_DEBUG_ALWAYS_USE_DERP=false",
	} {
		if !strings.Contains(text, required) {
			t.Errorf(".env.example missing %q", required)
		}
	}
}

func TestCommittedConfigurationUsesSanitizedPlaceholders(t *testing.T) {
	content, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, required := range []string{
		testAssignment("TS_AUTHKEY", "") + "\n",
		testAssignment("TS_LOGIN_SERVER", "https://headscale.example.com"),
		testAssignment("TAILGATE_TAILNET_SUFFIX", "ts.example.com"),
		testAssignment("HEADSCALE_USER", "user"),
	} {
		if !strings.Contains(text, required) {
			t.Errorf(".env.example missing sanitized placeholder %q", required)
		}
	}

	gitignore, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gitignore), "/autoinstall-user-data*") {
		t.Fatal(".gitignore must exclude /autoinstall-user-data* before repository discovery")
	}

	files := repositoryTextFiles(t)
	if _, ok := files["compose_test.go"]; !ok {
		t.Fatal("repository discovery omitted the sanitizer test itself")
	}
	accountID := regexp.MustCompile(`(^|[^0-9])[0-9]{12}([^0-9]|$)`)
	for name, content := range files {
		if accountID.Match(content) {
			t.Errorf("%s contains an AWS account-shaped identifier", name)
		}
		for _, finding := range unsafeAssignmentFindings(name, content) {
			t.Error(finding)
		}
	}
}

func repositoryTextFiles(t *testing.T) map[string][]byte {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("discover repository files: %v", err)
	}
	files := map[string][]byte{}
	for _, rawName := range bytes.Split(out, []byte{0}) {
		if len(rawName) == 0 {
			continue
		}
		name := string(rawName)
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read discovered file %s: %v", name, err)
		}
		if bytes.IndexByte(content, 0) >= 0 {
			continue
		}
		files[name] = content
	}
	return files
}

type sanitizerFinding struct {
	path string
	line int
	key  string
}

func (f sanitizerFinding) Error() string {
	return fmt.Sprintf("%s:%d contains unsafe %s assignment", f.path, f.line, f.key)
}

var safePlaceholderAssignments = map[string]map[string]bool{
	"TS_AUTHKEY": {
		"": true, "...": true, "<authkey>": true, "test-auth-key": true,
		`"test-auth-key"`: true, "tskey-auth-test": true, `'tskey-auth-test'`: true,
		"placeholder-auth-key": true, "ignored-by-fake": true,
	},
	"TS_LOGIN_SERVER": {
		"https://headscale.example.com": true, "http://192.0.2.1": true,
		"https://headscale.example.com/path with space": true,
	},
	"TAILGATE_TAILNET_SUFFIX": {
		"ts.example.com": true, "ts.test.example": true, "corp.ts.example.com": true,
		"file.ts.example.com": true, "environment.ts.example.com": true,
	},
	"HEADSCALE_USER": {"user": true},
}

func unsafeAssignmentFindings(path string, content []byte) []error {
	var findings []error
	for lineIndex, line := range strings.Split(string(content), "\n") {
		for key, approved := range safePlaceholderAssignments {
			prefix := key + "="
			for searchFrom := 0; searchFrom < len(line); {
				relativeIndex := strings.Index(line[searchFrom:], prefix)
				if relativeIndex < 0 {
					break
				}
				assignmentIndex := searchFrom + relativeIndex
				rhs := line[assignmentIndex+len(prefix):]
				if !approved[rhs] {
					findings = append(findings, sanitizerFinding{path: path, line: lineIndex + 1, key: key})
				}
				searchFrom = assignmentIndex + len(prefix)
			}
		}
	}
	return findings
}

func TestPlaceholderAssignmentClassifier(t *testing.T) {
	tests := []struct {
		name, input string
		safe        bool
	}{
		{name: "empty", input: "TS_AUTHKEY" + "=", safe: true},
		{name: "approved exact", input: "TS_AUTHKEY" + "=test-auth-key", safe: true},
		{name: "approved double quoted", input: "TS_AUTHKEY" + `="test-auth-key"`, safe: true},
		{name: "approved single quoted", input: "TS_AUTHKEY" + `='tskey-auth-test'`, safe: true},
		{name: "quoted sensitive", input: "TS_AUTHKEY" + `="synthetic-sensitive-value"`, safe: false},
		{name: "single quoted sensitive", input: "TS_AUTHKEY" + `='synthetic-private-value'`, safe: false},
		{name: "deceptive test substring", input: "TS_AUTHKEY" + "=synthetic-test-sensitive", safe: false},
		{name: "deceptive placeholder substring", input: "TS_AUTHKEY" + "=synthetic-placeholder-sensitive", safe: false},
		{name: "approved quoted plus trailing text", input: "TS_AUTHKEY" + `="test-auth-key"suffix`, safe: false},
		{name: "approved plus comment", input: "TS_AUTHKEY" + "=test-auth-key#comment", safe: false},
		{name: "braces", input: "TS_AUTHKEY" + "={test-auth-key}", safe: false},
		{name: "backslash escape", input: "TS_AUTHKEY" + `=test\-auth-key`, safe: false},
		{name: "escaped quotes", input: "TS_AUTHKEY" + `=\"test-auth-key\"`, safe: false},
		{name: "unmatched quote", input: "TS_AUTHKEY" + `="test-auth-key`, safe: false},
		{name: "multiple quotes", input: "TS_AUTHKEY" + `=""test-auth-key""`, safe: false},
		{name: "whitespace suffix", input: "TS_AUTHKEY" + "=test-auth-key extra", safe: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := unsafeAssignmentFindings("synthetic.txt", []byte(test.input))
			if got := len(findings) == 0; got != test.safe {
				t.Errorf("assignment classified safe = %v, want %v", got, test.safe)
			}
			if test.safe || len(findings) == 0 {
				return
			}
			rhs := strings.TrimPrefix(strings.TrimSpace(test.input), testAssignment("TS_AUTHKEY", ""))
			if strings.Contains(findings[0].Error(), rhs) {
				t.Fatal("sanitizer finding disclosed the assignment RHS")
			}
		})
	}
}

func TestSanitizerFindsAssignmentsInEveryTextContext(t *testing.T) {
	const sensitive = "synthetic-sensitive-value"
	assignment := "TS_AUTHKEY" + "=" + sensitive
	tests := []struct {
		name, line string
	}{
		{name: "commented", line: "# " + assignment},
		{name: "exported", line: "export " + assignment},
		{name: "double quoted context", line: `"` + assignment + `"`},
		{name: "single quoted context", line: `'` + assignment + `'`},
		{name: "indented", line: "    " + assignment},
		{name: "prose prefixed", line: "configure with " + assignment},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := unsafeAssignmentFindings("synthetic.txt", []byte(test.line))
			if len(findings) != 1 {
				t.Fatalf("findings = %d, want 1", len(findings))
			}
			message := findings[0].Error()
			if strings.Contains(message, sensitive) {
				t.Fatal("sanitizer finding disclosed the assignment value")
			}
			if !strings.Contains(message, "synthetic.txt") || !strings.Contains(message, "TS_AUTHKEY") {
				t.Fatalf("sanitizer finding lacks path/key category: %s", message)
			}
		})
	}
}

func TestGitExcludesOnlyRootAutoinstallArtifacts(t *testing.T) {
	for _, name := range []string{"autoinstall-user-data", "autoinstall-user-data.bak"} {
		cmd := exec.Command("git", "check-ignore", "--no-index", "--quiet", name)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("git does not ignore %s: %v\n%s", name, err, out)
		}
	}

	nested := filepath.Join("docs", "autoinstall-user-data-sanitizer-fixture.md")
	if err := os.WriteFile(nested, []byte("synthetic fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(nested) })
	cmd := exec.Command("git", "check-ignore", "--no-index", "--quiet", nested)
	if err := cmd.Run(); err == nil {
		t.Fatalf("root-anchored ignore unexpectedly hides %s", nested)
	}
	if _, ok := repositoryTextFiles(t)[nested]; !ok {
		t.Fatalf("repository discovery omitted nested similarly named file %s", nested)
	}
}

func TestDockerContextExcludesLocalSecretsAndState(t *testing.T) {
	content, err := os.ReadFile(".dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	patterns := map[string]bool{}
	for _, line := range strings.Split(string(content), "\n") {
		patterns[strings.TrimSpace(line)] = true
	}
	for _, required := range []string{".env*", ".git", "certs", "autoinstall-user-data*"} {
		if !patterns[required] {
			t.Errorf(".dockerignore does not exclude %q", required)
		}
	}
	for pattern := range patterns {
		if strings.HasPrefix(pattern, "!") && strings.Contains(pattern, ".env") {
			t.Errorf(".dockerignore re-includes an env file: %q", pattern)
		}
	}
}

func TestDockerfileUsesExactOverrideableBaseImages(t *testing.T) {
	content, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, required := range []string{
		"ARG GO_IMAGE=public.ecr.aws/docker/library/golang:1.24.13-alpine3.23",
		"ARG TAILSCALE_IMAGE=ghcr.io/tailscale/tailscale:v1.102.3",
		"FROM ${GO_IMAGE} AS build",
		"FROM ${TAILSCALE_IMAGE}",
		"ENTRYPOINT [\"/usr/local/bin/tailgate-entrypoint\"]",
		"CMD []",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("Dockerfile missing %q", required)
		}
	}

	composeContent, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if required := "GO_IMAGE: ${GO_IMAGE:-public.ecr.aws/docker/library/golang:1.24.13-alpine3.23}"; !strings.Contains(string(composeContent), required) {
		t.Errorf("compose.yaml missing %q", required)
	}
}

func TestForcedDERPDefaultsFalse(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "--env-file", ".env.example", "--profile", "userspace", "config", "--format", "json")
	cmd.Dir = root
	cmd.Env = isolatedTestEnv(testAssignment("TS_AUTHKEY", "test-auth-key"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatal(err)
	}
	service := object(t, object(t, config["services"])["tailscale-userspace"])
	if got := object(t, service["environment"])["TS_DEBUG_ALWAYS_USE_DERP"]; got != "false" {
		t.Fatalf("TS_DEBUG_ALWAYS_USE_DERP = %v, want false", got)
	}
}

func TestComposeProfilesDifferOnlyForTunCapabilityAndMode(t *testing.T) {
	userspace := object(t, object(t, composeConfig(t, "userspace")["services"])["tailscale-userspace"])
	kernel := object(t, object(t, composeConfig(t, "kernel")["services"])["tailscale-kernel"])
	if object(t, userspace["environment"])["TS_USERSPACE"] != "true" {
		t.Fatal("userspace profile does not set TS_USERSPACE=true")
	}
	if object(t, kernel["environment"])["TS_USERSPACE"] != "false" {
		t.Fatal("kernel profile does not set TS_USERSPACE=false")
	}
	if _, ok := userspace["cap_add"]; ok {
		t.Fatal("userspace profile has cap_add")
	}
	caps := array(t, kernel["cap_add"])
	if len(caps) != 1 || caps[0] != "NET_ADMIN" {
		t.Fatalf("kernel cap_add: %#v", caps)
	}
	devices := array(t, kernel["devices"])
	device := object(t, devices[0])
	if device["source"] != "/dev/net/tun" || device["target"] != "/dev/net/tun" {
		t.Fatalf("kernel device: %#v", device)
	}
}
