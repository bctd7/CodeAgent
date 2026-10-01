package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"CodeAgent/internal/agent"
	runtimeconfig "CodeAgent/internal/runtime/config"
	sessionconfig "CodeAgent/internal/session/config"
)

// 运行真正的测试子进程，避免用 goroutine 模拟操作系统锁。
func TestOwnershipChild(t *testing.T) {
	path := os.Getenv("CODEAGENT_TEST_DATABASE")
	if path == "" {
		return
	}
	m, err := NewManager(context.Background(), runtimeconfig.Config{DatabasePath: path, Models: testModels(), Stream: textStream, Tools: []agent.Tool{}})
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(2)
	}
	defer m.Close(context.Background())
	_, err = m.OpenSession(context.Background(), os.Getenv("CODEAGENT_TEST_SESSION"), runtimeconfig.OpenSessionOptions{})
	if errors.Is(err, runtimeconfig.ErrSessionOwned) {
		fmt.Println("OWNED")
		return
	}
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(2)
	}
	fmt.Println("READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

type childOwner struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  bool
}

func startChild(t *testing.T, database, id, expected string) *childOwner {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestOwnershipChild$")
	cmd.Env = append(os.Environ(), "CODEAGENT_TEST_DATABASE="+database, "CODEAGENT_TEST_SESSION="+id)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &childOwner{cmd: cmd, stdin: in}
	t.Cleanup(func() {
		if !child.done {
			_ = cmd.Process.Kill()
			_ = in.Close()
			_ = cmd.Wait()
		}
	})
	line := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		if scanner.Scan() {
			line <- scanner.Text()
		} else {
			line <- "no output"
		}
	}()
	select {
	case result := <-line:
		if result != expected {
			t.Fatalf("child result %q, expected %q", result, expected)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child initialization timeout")
	}
	return child
}

func (c *childOwner) exit(t *testing.T, kill bool) {
	t.Helper()
	if kill {
		if err := c.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.stdin.Close()
	err := c.cmd.Wait()
	c.done = true
	if !kill && err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipAcrossProcesses(t *testing.T) {
	m := newTestManager(t, textStream, []agent.Tool{})
	a, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: sessionconfig.ModelSelection{Ref: "local/a"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: sessionconfig.ModelSelection{Ref: "local/a"}})
	if err != nil {
		t.Fatal(err)
	}
	first := startChild(t, m.database, a.ID, "READY")
	occupied := startChild(t, m.database, a.ID, "OWNED")
	occupied.exit(t, false)
	other := startChild(t, m.database, b.ID, "READY")
	if _, err := m.ListSessions(context.Background(), sessionconfig.SessionListOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.ArchiveSession(context.Background(), a.ID); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatalf("foreign archive allowed: %v", err)
	}
	if err := m.RenameSession(context.Background(), a.ID, "foreign"); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatalf("foreign rename allowed: %v", err)
	}
	// Idle still holds ownership, orderly shutdown releases it.
	first.exit(t, false)
	r, err := m.OpenSession(context.Background(), a.ID, runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The OS releases a killed process's lock without deleting its lock file.
	other.exit(t, true)
	if _, err := m.OpenSession(context.Background(), b.ID, runtimeconfig.OpenSessionOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipPathAliasesAndInitializationFailure(t *testing.T) {
	m := newTestManager(t, textStream, []agent.Tool{})
	r := openTestSession(t, m)
	alias := filepath.Dir(m.database) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(m.database)
	canonical, err := canonicalDatabasePath(alias)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != m.database {
		t.Fatal("relative path alias changed identity")
	}
	if strings.Contains(m.database, ":") {
		canonical, err = canonicalDatabasePath(strings.ToUpper(m.database))
		if err != nil {
			t.Fatal(err)
		}
		if canonical != m.database {
			t.Fatal("Windows case alias changed identity")
		}
	}
	if _, err := acquireOwnership(context.Background(), canonical, r.SessionID()); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatal(err)
	}
	aliasConfig := m.config
	aliasConfig.DatabasePath = alias
	aliasManager, err := NewManager(context.Background(), aliasConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := aliasManager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := aliasManager.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{}); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatalf("path alias bypassed ownership: %v", err)
	}
	info, err := m.service.CreateSession(context.Background(), "legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{}); !errors.Is(err, runtimeconfig.ErrModelRequired) {
		t.Fatal(err)
	}
	// A failed initializer must not leave its registration or process-local lock.
	owner, err := acquireOwnership(context.Background(), m.database, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner.release()
	invalid := sessionconfig.ModelSelection{Ref: "local/missing"}
	if _, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{Model: &invalid}); err == nil {
		t.Fatal("invalid model opened")
	}
	owner, err = acquireOwnership(context.Background(), m.database, info.ID)
	if err != nil {
		t.Fatal("invalid model leaked ownership", err)
	}
	owner.release()
	selection := sessionconfig.ModelSelection{Ref: "local/a"}
	if _, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{Model: &selection}); err != nil {
		t.Fatal(err)
	}
	missing := "does-not-exist"
	if _, err := m.OpenSession(context.Background(), missing, runtimeconfig.OpenSessionOptions{}); err == nil {
		t.Fatal("missing opened")
	}
	owner, err = acquireOwnership(context.Background(), m.database, missing)
	if err != nil {
		t.Fatal(err)
	}
	owner.release()
	// Invalid tool initialization must also release ownership before return.
	info, err = m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: selection})
	if err != nil {
		t.Fatal(err)
	}
	old := m.config.Tools
	m.config.Tools = []agent.Tool{{}}
	if _, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{}); err == nil {
		t.Fatal("invalid tool accepted")
	}
	m.config.Tools = old
	if _, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{}); err != nil {
		t.Fatal(err)
	}
}
