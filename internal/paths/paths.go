package paths

import (
	"os"
	"path/filepath"
)

func Root() string {
	if v := os.Getenv("BIGBAND_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		panic("bigband: cannot determine home directory: " + err.Error())
	}
	return filepath.Join(home, ".bigband-tasks")
}

func Config() string       { return filepath.Join(Root(), "config.yaml") }
func Socket() string       { return filepath.Join(Root(), "daemon.sock") }
func PidFile() string      { return filepath.Join(Root(), "daemon.pid") }
func InstanceLock() string { return filepath.Join(Root(), "daemon.lock") }
func DaemonLog() string    { return filepath.Join(Root(), "daemon.log") }
func StateFile() string { return filepath.Join(Root(), "state.json") }
func StateDir() string  { return filepath.Join(Root(), "state") }
func LogsDir() string   { return filepath.Join(Root(), "logs") }

func TaskLogDir(name string) string { return filepath.Join(LogsDir(), name) }
func TaskLockFile(name string) string {
	return filepath.Join(StateDir(), name+".lock")
}
func TaskLogLatest(name string) string {
	return filepath.Join(TaskLogDir(name), "latest.log")
}

// EventsFile is the JSONL append-only log of lifecycle events emitted by the
// daemon. Used by integrations as a durable ground-truth event stream.
func EventsFile() string { return filepath.Join(Root(), "events.jsonl") }

func EnsureDirs() error {
	for _, d := range []string{Root(), StateDir(), LogsDir()} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	return nil
}
