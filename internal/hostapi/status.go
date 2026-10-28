package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxField = 256

func validString(s string) bool { return len(s) <= maxField && !strings.ContainsAny(s, "\x00\r\n") }

// Validate bounds every host-supplied field. The agent never trusts the size or
// shape of what arrives over the channel, even though the channel is mTLS.
func (f Facts) Validate() error {
	for _, s := range []string{f.HostTime, f.AppVersion, f.ApplianceVersion, f.BundleID, f.K3sVersion, f.Arch, f.OS,
		f.Autostart.Mechanism, f.Autostart.Note, f.Update.Phase, f.Update.Kind, f.Update.CandidateVersion,
		f.Update.AvailableVersion, f.Update.BlockedReason, f.Gateway.RootCAURL, f.Gateway.RootCAFingerprintSHA256} {
		if !validString(s) {
			return errors.New("invalid host fact string")
		}
	}
	if _, err := time.Parse(time.RFC3339, f.HostTime); err != nil {
		return errors.New("hostTime must be RFC 3339")
	}
	if err := validateDestinations(f.Destinations); err != nil {
		return err
	}
	if err := validateRecovery(f); err != nil {
		return err
	}
	if len(f.Update.RetainedRecoveryPoints) > 32 || len(f.Gateway.BoundAddresses) > 32 {
		return errors.New("too many list entries")
	}
	for _, s := range append(append([]string{}, f.Update.RetainedRecoveryPoints...), f.Gateway.BoundAddresses...) {
		if !validString(s) {
			return errors.New("invalid host fact string")
		}
	}
	if lan := f.Gateway.LANCheck; lan != nil {
		if len(lan.Results) > 32 || !validString(lan.CheckedAt) || !validString(lan.Note) {
			return errors.New("invalid LAN check")
		}
		for _, r := range lan.Results {
			if !validString(r.Address) || !validString(r.Error) {
				return errors.New("invalid LAN check")
			}
		}
	}
	for _, n := range []int64{f.CPU.Allocated, f.CPU.HostTotal, f.MemoryMiB.Allocated, f.MemoryMiB.HostTotal, f.StateDirFreeBytes, f.StateDirTotal} {
		if n < 0 {
			return errors.New("negative host fact")
		}
	}
	for _, p := range []int{f.Gateway.HTTPPort, f.Gateway.HTTPSPort} {
		if p < 0 || p > 65535 {
			return errors.New("invalid port")
		}
	}
	return nil
}

// DiskFromStatfs derives the data-disk figures and the pressure flag. Pressure
// flips when free space falls under the larger of 1 GiB and 10 percent of the disk.
func DiskFromStatfs(size, free int64) Disk {
	if size < 0 {
		size = 0
	}
	if free < 0 {
		free = 0
	}
	if free > size {
		free = size
	}
	d := Disk{SizeBytes: size, FreeBytes: free, UsedBytes: size - free}
	floor := size / 10
	if floor < 1<<30 {
		floor = 1 << 30
	}
	if size > 0 && free < floor {
		d.Pressure = true
		d.PressureReason = fmt.Sprintf("%d bytes free is below the %d byte threshold", free, floor)
	}
	return d
}

// Build merges host facts and guest facts. host may be nil (never received).
func Build(now time.Time, host *Facts, hostAt time.Time, guest GuestFacts) Status {
	s := Status{ObservedAt: now.UTC().Format(time.RFC3339), Guest: guest}
	cond := Condition{Type: "HostReported", Status: "Unknown", Reason: "NoHostFacts", Message: "The launcher has not reported yet"}
	if host != nil {
		age := now.Sub(hostAt)
		if age < 0 {
			age = 0
		}
		secs := int64(age / time.Second)
		s.HostFactsAgeSeconds = &secs
		if age <= StaleAfter {
			h := *host
			s.Host = &h
			cond = Condition{Type: "HostReported", Status: "True", Reason: "Fresh", Message: "Host facts are current"}
			if t, err := time.Parse(time.RFC3339, host.HostTime); err == nil {
				skew := hostAt.Sub(t).Seconds()
				s.ClockSkewSeconds = &skew
			}
		} else {
			cond = Condition{Type: "HostReported", Status: "Unknown", Reason: "Stale",
				Message: fmt.Sprintf("Host facts are %ds old; showing no host values", secs)}
		}
	}
	s.Conditions = []Condition{cond}
	pressure := Condition{Type: "DiskPressure", Status: "False", Reason: "Sufficient", Message: "The data disk has enough free space"}
	if guest.Disk.Pressure {
		pressure = Condition{Type: "DiskPressure", Status: "True", Reason: "Low", Message: guest.Disk.PressureReason}
	}
	s.Conditions = append(s.Conditions, pressure)
	return s
}

// MaterialKey identifies the content that matters for deciding whether to write
// the status again: it ignores the time-varying fields.
func (s Status) MaterialKey() string {
	c := s
	c.ObservedAt = ""
	c.HostFactsAgeSeconds = nil
	c.ClockSkewSeconds = nil
	c.Guest.UptimeSeconds = 0
	if c.Host != nil {
		h := *c.Host
		h.HostTime = ""
		h.Destinations = nil
		c.Host = &h
	}
	b, _ := json.Marshal(c)
	return string(b)
}
