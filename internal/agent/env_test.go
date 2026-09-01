package agent

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestMergeEnvSortsForDeterminism(t *testing.T) {
	got := MergeEnv([]string{"A=1"}, map[string]string{"C": "3", "B": "2"})
	want := []string{"A=1", "B=2", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	// Same input, same output — a provider that ranged the map directly would
	// produce a different order on some runs.
	for i := 0; i < 20; i++ {
		if again := MergeEnv([]string{"A=1"}, map[string]string{"C": "3", "B": "2"}); !reflect.DeepEqual(again, want) {
			t.Fatalf("iteration %d differed: %v", i, again)
		}
	}
}

func TestMergeEnvAppendsAfterBaseSoOverridesWin(t *testing.T) {
	got := MergeEnv([]string{"FOO=old", "BAR=keep"}, map[string]string{"FOO": "new"})
	want := []string{"FOO=old", "BAR=keep", "FOO=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("override must be appended after the base; want %v, got %v", want, got)
	}
}

func TestMergeEnvEmptyExtraReturnsBaseUnchanged(t *testing.T) {
	base := []string{"A=1"}
	if got := MergeEnv(base, nil); !reflect.DeepEqual(got, base) {
		t.Errorf("nil extra should return base unchanged, got %v", got)
	}
	if got := MergeEnv(base, map[string]string{}); !reflect.DeepEqual(got, base) {
		t.Errorf("empty extra should return base unchanged, got %v", got)
	}
}

func TestMergeEnvDoesNotMutateBase(t *testing.T) {
	base := []string{"A=1"}
	_ = MergeEnv(base, map[string]string{"B": "2"})
	if len(base) != 1 || base[0] != "A=1" {
		t.Fatalf("base was mutated: %v", base)
	}
}

// MergeEnv's whole correctness rests on Go's exec honouring the LAST duplicate
// key. That is a platform/stdlib behaviour rather than something this package
// controls, so pin it with a real subprocess: if it ever changed, appending
// would stop overriding and every job would silently get the inherited value.
func TestExecHonoursLastDuplicateKey(t *testing.T) {
	t.Setenv("BB_MERGE_PROBE", "inherited")
	c := exec.Command("printenv", "BB_MERGE_PROBE")
	c.Env = MergeEnv(os.Environ(), map[string]string{"BB_MERGE_PROBE": "overridden"})
	out, err := c.Output()
	if err != nil {
		t.Fatalf("printenv: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "overridden" {
		t.Fatalf("appended value must win in exec; got %q", got)
	}
}

// Setting a key to "" yields an empty value, not an unset variable — worth
// pinning so nobody assumes env: can remove an inherited variable.
func TestMergeEnvEmptyValueIsEmptyNotUnset(t *testing.T) {
	t.Setenv("BB_MERGE_BLANK", "inherited")
	c := exec.Command("sh", "-c", "printenv BB_MERGE_BLANK; echo exit=$?")
	c.Env = MergeEnv(os.Environ(), map[string]string{"BB_MERGE_BLANK": ""})
	out, _ := c.Output()
	if !strings.Contains(string(out), "exit=0") {
		t.Fatalf("variable should still be set (empty), got %q", string(out))
	}
	if strings.Contains(string(out), "inherited") {
		t.Fatalf("empty override should replace the inherited value, got %q", string(out))
	}
}
