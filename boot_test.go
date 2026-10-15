package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBootDiagnosisReportsWHPXAndStaysBounded(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "console.log"), []byte("qemu: WHPX: Unexpected VP exit code 4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := bootDiagnosis(state); !strings.Contains(got, "WHPX") {
		t.Fatal("did not surface hypervisor failure:", got)
	}
	if err := os.Remove(filepath.Join(state, "console.log")); err != nil {
		t.Fatal(err)
	}
	if got := bootDiagnosis(state); !strings.Contains(got, "boot timeout") {
		t.Fatal("missing bounded failure report:", got)
	}
	// Never block boot detection longer than the configured allowance.
	t.Setenv("CONTAINERS_BOOT_TIMEOUT", "1h")
	if got := bootTimeout(); got != time.Hour {
		t.Fatal(got)
	}
	t.Setenv("CONTAINERS_BOOT_TIMEOUT", "nonsense")
	if got := bootTimeout(); got != 45*time.Minute {
		t.Fatal(got)
	}
}
