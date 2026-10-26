// Package protocol defines host/guest control compatibility, not disk migrations.
package protocol

import "slices"

const Version = 1

func Capabilities() []string {
	return []string{"status", "logs", "stop", "kite-proxy", "gateway-v1"}
}

func Supports(version int, capabilities []string) bool {
	if version != Version {
		return false
	}
	for _, required := range []string{"status", "logs", "stop", "kite-proxy"} {
		if !slices.Contains(capabilities, required) {
			return false
		}
	}
	return true
}
