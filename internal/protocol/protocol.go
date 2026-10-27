// Package protocol defines host/guest control compatibility, not disk migrations.
package protocol

import "slices"

const Version = 1

// DataContract names the persistent data layout under /var/lib. A root image may
// only run on a disk that carries exactly the contract it supports.
const DataContract = "var-lib-v1"

func Capabilities() []string {
	return []string{"status", "logs", "stop", "console-proxy", "platform-apply-v1", "workload-publication-v1", "host-api-v1"}
}

func Supports(version int, capabilities []string) bool {
	if version != Version {
		return false
	}
	for _, required := range Capabilities() {
		if !slices.Contains(capabilities, required) {
			return false
		}
	}
	return true
}
