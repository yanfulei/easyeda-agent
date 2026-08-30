package app

import (
	"bytes"
	"testing"
	"time"
)

func TestCatalogActionTimeout(t *testing.T) {
	if got := catalogActionTimeout("schematic.components.list"); got != 60*time.Second {
		t.Fatalf("ordinary action timeout = %v, want 60s", got)
	}
	for _, action := range []string{"schematic.save", "pcb.drc.check", "pcb.export.gerber"} {
		if got := catalogActionTimeout(action); got != 120*time.Second {
			t.Errorf("%s timeout = %v, want 120s", action, got)
		}
	}
	if got := catalogActionTimeout("future.unknown"); got != defaultActionTimeout {
		t.Fatalf("unknown action timeout = %v, want fallback %v", got, defaultActionTimeout)
	}
}

func TestGenericCallExposesTimeoutOverride(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := newCallCmd(&appConfig{}, &out, &errOut)
	flag := cmd.Flags().Lookup("timeout")
	if flag == nil {
		t.Fatal("easyeda call must expose --timeout for MCP/generic callers")
	}
	if flag.DefValue != "0s" {
		t.Fatalf("--timeout default = %q, want 0s so catalog timeout applies", flag.DefValue)
	}
}
