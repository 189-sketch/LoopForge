package deploy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBindCLIMacInstallsAndUpgradesDockerRunnerHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installer runs on macOS and Linux")
	}
	for unameArch, arch := range map[string]string{"arm64": "arm64", "x86_64": "amd64"} {
		t.Run(arch, func(t *testing.T) {
			home := t.TempDir()
			fakeBin := filepath.Join(home, "fake-bin")
			binDir := filepath.Join(home, "runner-bin")
			if err := os.Mkdir(fakeBin, 0o755); err != nil {
				t.Fatal(err)
			}
			uname := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo Darwin ;; -m) echo %s ;; esac\n", unameArch)
			if err := os.WriteFile(filepath.Join(fakeBin, "uname"), []byte(uname), 0o755); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				switch r.URL.Path {
				case "/runners/health":
					w.WriteHeader(http.StatusOK)
				case "/runners/agentflow-runner", "/runners/agentflow-gate-mcp":
					fmt.Fprintf(w, "%s %s/%s", filepath.Base(r.URL.Path), query.Get("os"), query.Get("arch"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			runInstaller := func(args ...string) {
				t.Helper()
				cmd := exec.Command("/bin/bash", append([]string{"bind-cli.sh", "--bin-dir", binDir}, args...)...)
				cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fakeBin+":/usr/bin:/bin", "AGENTFLOW_BROWSER=off", "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("installer %v: %v\n%s", args, err, output)
				}
			}
			helper := filepath.Join(binDir, "agentflow-runner-linux-"+arch)
			checkHelper := func() {
				t.Helper()
				for path, want := range map[string]string{
					filepath.Join(binDir, "agentflow-runner"): "agentflow-runner darwin/" + arch,
					helper: "agentflow-runner linux/" + arch,
				} {
					if data, err := os.ReadFile(path); err != nil || string(data) != want {
						t.Fatalf("%s = %q, %v, want %q", path, data, err, want)
					}
					if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o111 == 0 {
						t.Fatalf("%s is not executable: %v", path, err)
					}
				}
			}

			runInstaller("--server", server.URL, "--token", "test-binding", "--no-start")
			checkHelper()
			state := filepath.Join(home, ".agentflow-runner", "state.json")
			if err := os.WriteFile(state, []byte("existing binding"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(helper, []byte("old helper"), 0o755); err != nil {
				t.Fatal(err)
			}
			runInstaller("--upgrade")
			checkHelper()
			if data, err := os.ReadFile(state); err != nil || string(data) != "existing binding" {
				t.Fatalf("upgrade changed existing binding: %q, %v", data, err)
			}
		})
	}
}
