// Package protocol defines host/guest control compatibility, not disk migrations.
package protocol

import "slices"

const Version = 1

func Capabilities() []string {
	return []string{"status", "logs", "stop", "kite-proxy", "gateway-v1", "cncm-bootstrap"}
}

func Supports(version int, capabilities []string) bool {
	if version != Version {
		return false
	}
	// Published alpha manifests predate explicit capabilities. Protocol 1
	// guarantees the core controls; it does not guarantee optional gateways.
	if capabilities == nil {
		return true
	}
	for _, required := range []string{"status", "logs", "stop", "kite-proxy"} {
		if !slices.Contains(capabilities, required) {
			return false
		}
	}
	return true
}
