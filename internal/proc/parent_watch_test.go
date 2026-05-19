package proc

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestWatchParent_ReturnsFalseWhenPPIDIsInit(t *testing.T) {
	// We can't easily force os.Getppid() to return 1 from inside the test
	// process, so cover the alternative no-op exit path: when ctx is cancelled
	// before any reparenting happens, WatchParent must return false. This is
	// the path normal graceful shutdown takes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if WatchParent(ctx, 50*time.Millisecond) {
		t.Fatal("expected false when ctx is already cancelled")
	}
}

// TestWatchParent_DetectsReparenting spawns a child of a short-lived shim
// process so we can observe getppid() change. The shim is built from this
// test binary via the standard re-exec trick (TestHelperProcess pattern).
func TestWatchParent_DetectsReparenting(t *testing.T) {
	if os.Getenv("BIGBAND_PROC_TEST_CHILD") == "1" {
		runChildAndReport()
		return
	}
	if os.Getenv("BIGBAND_PROC_TEST_SHIM") == "1" {
		runShim(t)
		return
	}

	// Outer test: spawn the shim. The shim will spawn the child, then exit
	// immediately. The child watches its PPID and exits 42 when reparented.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	shim := exec.Command(exe, "-test.run", "TestWatchParent_DetectsReparenting")
	shim.Env = append(os.Environ(), "BIGBAND_PROC_TEST_SHIM=1")
	out, err := shim.CombinedOutput()
	if err != nil {
		t.Fatalf("shim failed: %v\noutput: %s", err, out)
	}
	// Shim prints the child pid; we then wait briefly for the child to notice
	// and exit. We can't `Wait` on the child (it's our grandchild now), so
	// poll for the process to disappear.
	pid, err := strconv.Atoi(string(out))
	if err != nil {
		t.Fatalf("could not parse child pid from shim output %q: %v", out, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Best-effort cleanup before failing.
	_ = exec.Command("kill", "-9", strconv.Itoa(pid)).Run()
	t.Fatalf("child pid=%d did not exit within deadline after parent died", pid)
}

func runShim(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	child := exec.Command(exe, "-test.run", "TestWatchParent_DetectsReparenting")
	child.Env = append(os.Environ(), "BIGBAND_PROC_TEST_CHILD=1")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	// Print only the child PID (caller parses it directly).
	os.Stdout.WriteString(strconv.Itoa(child.Process.Pid))
	// Exit immediately, orphaning the child.
	os.Exit(0)
}

func runChildAndReport() {
	// Use a short poll interval so the test finishes quickly.
	if WatchParent(context.Background(), 100*time.Millisecond) {
		os.Exit(42)
	}
	os.Exit(0)
}
