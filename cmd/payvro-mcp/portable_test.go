package main_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCmdCrossCompiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		goos   string
		goarch string
	}{
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
		{goos: "windows", goarch: "amd64"},
		{goos: "windows", goarch: "arm64"},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			t.Parallel()
			out := filepath.Join(t.TempDir(), "payvro-mcp")
			cmd := exec.Command("go", "build", "-o", out, ".")
			cmd.Env = append(os.Environ(), "GOOS="+tc.goos, "GOARCH="+tc.goarch)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("GOOS=%s GOARCH=%s go build: %v\n%s", tc.goos, tc.goarch, err, b)
			}
		})
	}
}

func TestPanicPathRedactsCanaryOnStderr(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "payvro-mcp")
	build := exec.Command("go", "build", "-tags", "pv5testpanic", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -tags pv5testpanic: %v\n%s", err, out)
	}

	t.Run("panic prints redacted canary and exits 2", func(t *testing.T) {
		const bffCanary = "synthetic-bff-shared-secret-canary"
		stdout, stderr, code, timedOut := runUntilExit(t, bin, []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"PEY_API_KEY=" + outputCanary,
			"PAYVRO_BFF_SHARED_SECRET=" + bffCanary,
			"PAYVRO_TEST_PANIC=1",
		})
		if timedOut {
			t.Fatalf("panic path did not exit\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		if code != 2 {
			t.Fatalf("exit %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if strings.Contains(stdout, outputCanary) || strings.Contains(stderr, outputCanary) || strings.Contains(stdout, bffCanary) || strings.Contains(stderr, bffCanary) {
			t.Fatalf("canary leaked\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		const want = "payvro-mcp: panic: payvro-mcp test panic: [redacted] [redacted]\n"
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing redacted panic line %q\nstderr: %s", want, stderr)
		}
	})

	t.Run("tag without the env flag keeps serving", func(t *testing.T) {
		stdout, stderr, code, timedOut := runUntilExit(t, bin, []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"PEY_API_KEY=" + outputCanary,
		})
		if !timedOut {
			t.Fatalf("exited %d without PAYVRO_TEST_PANIC\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if strings.Contains(stdout, outputCanary) || strings.Contains(stderr, outputCanary) {
			t.Fatalf("canary leaked while idle\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	})
}

func runUntilExit(t *testing.T, bin string, env []string) (stdout, stderr string, code int, timedOut bool) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code = 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("wait: %v", err)
			}
			code = exitErr.ExitCode()
		}
	case <-time.After(3 * time.Second):
		timedOut = true
		_ = cmd.Process.Kill()
		<-done
	}
	return outBuf.String(), errBuf.String(), code, timedOut
}
