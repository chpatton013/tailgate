package tailgate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteEnvUsesOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are required")
	}
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte(testAssignment("TS_AUTHKEY", "")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	python := strings.Join([]string{
		"import runpy, sys",
		"from pathlib import Path",
		"module = runpy.run_path('bin/mint-authkey')",
		"module['write_env'].__globals__['ENV_FILE'] = Path(sys.argv[1])",
		"module['write_env']('placeholder-auth-key')",
	}, "\n")
	cmd := exec.Command("python3", "-c", python, envFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write_env failed: %v\n%s", err, out)
	}
	info, err := os.Stat(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf(".env permissions = %04o, want %04o", got, want)
	}
}
