package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDaemonProcessLogsExitBoundary(t *testing.T) {
	tests := []struct {
		name      string
		runErr    error
		wantCode  int
		wantLevel string
		wantAttrs []string
	}{
		{
			name:      "success logs exit_code 0",
			runErr:    nil,
			wantCode:  0,
			wantLevel: "level=INFO",
			wantAttrs: []string{`reason="daemon run returned"`, "exit_code=0"},
		},
		{
			name:      "run error logs exit_code 1",
			runErr:    errors.New("boom"),
			wantCode:  1,
			wantLevel: "level=ERROR",
			wantAttrs: []string{`reason="daemon run error"`, "exit_code=1", `error=boom`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(old)

			oldRun := daemonRun
			daemonRun = func() error { return tt.runErr }
			defer func() { daemonRun = oldRun }()

			code := runDaemonProcess("")
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
			}

			got := logs.String()
			wants := append([]string{`msg="daemon process exiting"`, tt.wantLevel}, tt.wantAttrs...)
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Fatalf("exit log missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestCLILogWriterReturnsDiscardWhenLogsDirMissing(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	w := cliLogWriter()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(nmHome, "logs", "cli.log")); !os.IsNotExist(err) {
		t.Fatalf("cli.log should not be created when logs dir is missing, stat err = %v", err)
	}

	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}
}

func TestCLILogWriterAppendsToFileWhenLogsDirExists(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	logsDir := filepath.Join(nmHome, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	w := cliLogWriter()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if c, ok := w.(io.Closer); ok {
		if err := c.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	}

	b, err := os.ReadFile(filepath.Join(logsDir, "cli.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("cli.log contents = %q, want %q", string(b), "hello\n")
	}
}

func TestDaemonRunRootFromArgs(t *testing.T) {
	t.Setenv("NM_DAEMON", "")

	tests := []struct {
		name     string
		args     []string
		wantRoot string
		wantOK   bool
		wantErr  string
	}{
		{name: "non-daemon command", args: []string{"daemon", "status"}},
		{name: "daemon run no root", args: []string{"daemon", "run"}, wantOK: true},
		{name: "daemon run root flag", args: []string{"daemon", "run", "--root", "/tmp/nm"}, wantRoot: "/tmp/nm", wantOK: true},
		{name: "daemon run root equals", args: []string{"daemon", "run", "--root=/tmp/nm"}, wantRoot: "/tmp/nm", wantOK: true},
		{name: "daemon run missing root value", args: []string{"daemon", "run", "--root"}, wantErr: "missing value for --root"},
		{name: "daemon run help", args: []string{"daemon", "run", "--help"}},
		{name: "daemon run short help", args: []string{"daemon", "run", "-h"}},
		{name: "daemon run unknown flag", args: []string{"daemon", "run", "--bogus"}},
		{name: "daemon run extra arg", args: []string{"daemon", "run", "extra"}},
		{name: "daemon run root plus extra arg", args: []string{"daemon", "run", "--root", "/tmp/nm", "extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRoot, gotOK, err := daemonRunRootFromArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if gotRoot != tt.wantRoot || gotOK != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", gotRoot, gotOK, tt.wantRoot, tt.wantOK)
			}
		})
	}
}

func TestDaemonRunRootFromArgs_EnvForcesDaemonMode(t *testing.T) {
	t.Setenv("NM_DAEMON", "1")

	gotRoot, gotOK, err := daemonRunRootFromArgs([]string{"status"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotRoot != "" || !gotOK {
		t.Fatalf("got (%q, %v), want (%q, %v)", gotRoot, gotOK, "", true)
	}
}
