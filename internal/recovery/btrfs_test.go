package recovery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareSimpleQuotasFreshAndRepeatedEnable(t *testing.T) {
	for _, initialMode := range []string{"", "squota"} {
		t.Run("initial-"+initialMode, func(t *testing.T) {
			mode := initialMode
			var calls [][]string
			command := func(_ context.Context, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				switch strings.Join(args, " ") {
				case "quota enable --simple /var/lib":
					// Kernel quota enable is an idempotent no-op if a quota root
					// already exists; it does not convert an existing mode.
					if mode == "" {
						mode = "squota"
					}
					return nil, nil
				case "quota status /var/lib":
					return []byte(fmt.Sprintf("Quotas on /var/lib:\n  Enabled: yes\n  Mode: %s (simplified accounting)\n", mode)), nil
				case "filesystem sync /var/lib":
					return nil, nil
				default:
					t.Fatalf("unexpected quota command (no fallback/disable allowed): %v", args)
					return nil, errors.New("unexpected command")
				}
			}
			for range 2 {
				if err := prepareSimpleQuotas(context.Background(), "/var/lib", command); err != nil {
					t.Fatal(err)
				}
			}
			want := [][]string{
				{"quota", "enable", "--simple", "/var/lib"}, {"quota", "status", "/var/lib"}, {"filesystem", "sync", "/var/lib"},
				{"quota", "enable", "--simple", "/var/lib"}, {"quota", "status", "/var/lib"}, {"filesystem", "sync", "/var/lib"},
			}
			if mode != "squota" || !reflect.DeepEqual(calls, want) {
				t.Fatalf("wrong quota preparation sequence: mode=%s calls=%v", mode, calls)
			}
		})
	}
}

func TestPrepareSimpleQuotasRejectsUnsupportedModeWithoutConversion(t *testing.T) {
	for _, status := range []string{
		"Enabled: yes\nMode: qgroup (full accounting)\n",
		"Enabled: no\n",
		"Enabled: yes\n",
		"Enabled: yes\nMode: unknown\n",
	} {
		t.Run(strings.ReplaceAll(status, "\n", "/"), func(t *testing.T) {
			var calls []string
			command := func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				calls = append(calls, call)
				switch call {
				case "quota enable --simple /var/lib":
					return nil, nil // Existing full qgroups also return success.
				case "quota status /var/lib":
					return []byte(status), nil
				default:
					t.Fatalf("quota mode fallback/conversion attempted: %v", args)
					return nil, errors.New("unexpected command")
				}
			}
			if err := prepareSimpleQuotas(context.Background(), "/var/lib", command); err == nil {
				t.Fatal("enable success silently accepted a non-simple/unknown quota mode")
			}
			if len(calls) != 2 {
				t.Fatal(calls)
			}
		})
	}
}

func TestPrepareSimpleQuotasPropagatesEnableAndSyncFailures(t *testing.T) {
	for _, failureAt := range []string{"quota enable --simple /var/lib", "filesystem sync /var/lib"} {
		t.Run(failureAt, func(t *testing.T) {
			failure := errors.New("injected quota preparation failure")
			command := func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				if call == failureAt {
					return nil, failure
				}
				if call == "quota enable --simple /var/lib" {
					return nil, nil
				}
				if call == "quota status /var/lib" {
					return []byte("Enabled: yes\nMode: squota (simplified accounting)\n"), nil
				}
				t.Fatalf("unexpected command after failure: %v", args)
				return nil, errors.New("unexpected command")
			}
			if err := prepareSimpleQuotas(context.Background(), "/var/lib", command); !errors.Is(err, failure) {
				t.Fatal(err)
			}
		})
	}
}
