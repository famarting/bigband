package proc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireInstanceLock_AcquireReleaseRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.lock")

	release, holder, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: err=%v holder=%d", err, holder)
	}
	if release == nil {
		t.Fatal("first acquire returned nil release")
	}

	// The PID written into the file must be ours, so a second process
	// inspecting it can produce a useful error message.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("lock file is empty; expected pid")
	}

	release()
}

func TestAcquireInstanceLock_RejectsSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.lock")

	release1, _, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release1()

	release2, holder, err := AcquireInstanceLock(path)
	if err == nil {
		release2()
		t.Fatal("second acquire unexpectedly succeeded")
	}
	if holder != os.Getpid() {
		t.Fatalf("expected holder pid %d, got %d", os.Getpid(), holder)
	}
}

func TestAcquireInstanceLock_ReleaseAllowsReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.lock")

	release, _, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()

	release2, _, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("reacquire after release failed: %v", err)
	}
	release2()
}
