package protocol

import "testing"

// The capability set the published 0.2.0 appliance advertises. A newer app must keep
// running it and updating it: that is how an installed appliance gets replaced.
var published020 = []string{"status", "logs", "stop", "console-proxy", "platform-apply-v1", "workload-publication-v1"}

func TestCurrentAndEarlierAppliancesAreSupported(t *testing.T) {
	if !Supports(Version, Capabilities()) {
		t.Fatal("a current appliance must be supported")
	}
	if !Supports(Version, published020) {
		t.Fatal("an appliance without the optional host API must still run and be updatable")
	}
}

func TestMissingRequiredCapabilityOrWrongVersionIsRefused(t *testing.T) {
	for _, drop := range Required() {
		var caps []string
		for _, c := range Capabilities() {
			if c != drop {
				caps = append(caps, c)
			}
		}
		if Supports(Version, caps) {
			t.Errorf("accepted an appliance without required capability %s", drop)
		}
	}
	if Supports(Version+1, Capabilities()) || Supports(0, Capabilities()) || Supports(Version, nil) {
		t.Fatal("wrong protocol versions and empty capabilities must be refused")
	}
}

func TestOptionalAndRequiredDoNotOverlap(t *testing.T) {
	for _, o := range Optional() {
		if Has(Required(), o) {
			t.Fatalf("%s is both required and optional", o)
		}
	}
	if !Has(Capabilities(), "host-api-v1") || Has(published020, "host-api-v1") || !Has(Capabilities(), "storage-v1") {
		t.Fatal("host-api-v1 is optional and was added after 0.2.0")
	}
}
