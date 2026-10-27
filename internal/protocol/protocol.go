// Package protocol defines host/guest control compatibility, not disk migrations.
package protocol

import "slices"

const Version = 1

// DataContract names the persistent data layout under /var/lib. A root image may
// only run on a disk that carries exactly the contract it supports.
const DataContract = "var-lib-btrfs-v1"

// Required capabilities must be advertised by every supported appliance. Removing
// or adding one here strands existing appliances, so it needs a protocol version.
func Required() []string {
	return []string{"status", "logs", "stop", "console-proxy", "platform-apply-v1", "workload-publication-v1"}
}

// Optional capabilities add features. An app must keep working, with that feature
// off, against an appliance that lacks them: apps update far more often than the
// appliance, and a new feature must never strand an appliance that is already
// installed or make it impossible to update.
func Optional() []string { return []string{"host-api-v1", "storage-v1"} }

// Capabilities is what a current appliance advertises.
func Capabilities() []string { return append(Required(), Optional()...) }

// Supports reports whether an appliance can be run at all. It checks only the
// required capabilities.
func Supports(version int, capabilities []string) bool {
	if version != Version {
		return false
	}
	for _, required := range Required() {
		if !slices.Contains(capabilities, required) {
			return false
		}
	}
	return true
}

// Has reports whether an appliance advertises a capability.
func Has(capabilities []string, name string) bool { return slices.Contains(capabilities, name) }
