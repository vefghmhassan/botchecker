package config

import (
	"strings"
	"testing"
)

func setMinimum(t *testing.T) {
	t.Helper()
	t.Setenv("DASHBOARD_PASS", "s3cret")
}

func TestLoadRefusesWithoutDashboardPassword(t *testing.T) {
	t.Setenv("DASHBOARD_PASS", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with no dashboard password; " +
			"the service must not come up unauthenticated")
	}
	if !strings.Contains(err.Error(), "DASHBOARD_PASS") {
		t.Errorf("error = %v, want it to name DASHBOARD_PASS", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	setMinimum(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(c.IRNodes) != 8 {
		t.Errorf("IRNodes = %v, want the 8 Iranian probes", c.IRNodes)
	}
	// Short names are expanded to the hostnames check-host expects.
	if c.IRNodes[0] != "ir1.node.check-host.net" {
		t.Errorf("IRNodes[0] = %q, want ir1.node.check-host.net", c.IRNodes[0])
	}
	if !c.ControlEnabled() {
		t.Error("control nodes should be on by default")
	}
	if got := len(c.AllNodes()); got != 10 {
		t.Errorf("AllNodes() = %d, want 10", got)
	}
	if c.MinIRFail != 6 {
		t.Errorf("MinIRFail = %d, want 6", c.MinIRFail)
	}
}

func TestControlNodesCanBeDisabled(t *testing.T) {
	setMinimum(t)
	t.Setenv("CONTROL_NODES", " ")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ControlEnabled() {
		t.Error("ControlEnabled() = true after clearing CONTROL_NODES")
	}
}

func TestFullyQualifiedNodeNamesPassThrough(t *testing.T) {
	setMinimum(t)
	t.Setenv("IR_NODES", "ir1,ir2.node.check-host.net,probe.example.com")
	t.Setenv("MIN_IR_FAIL", "2")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"ir1.node.check-host.net", "ir2.node.check-host.net", "probe.example.com"}
	for i, w := range want {
		if c.IRNodes[i] != w {
			t.Errorf("IRNodes[%d] = %q, want %q", i, c.IRNodes[i], w)
		}
	}
}

func TestMinIRFailMustFitTheNodeSet(t *testing.T) {
	setMinimum(t)
	t.Setenv("IR_NODES", "ir1,ir2")
	t.Setenv("MIN_IR_FAIL", "6")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a threshold larger than the node count")
	}
	if !strings.Contains(err.Error(), "MIN_IR_FAIL") {
		t.Errorf("error = %v, want it to name MIN_IR_FAIL", err)
	}
}

func TestInvalidDurationIsReported(t *testing.T) {
	setMinimum(t)
	t.Setenv("SCAN_INTERVAL", "half an hour")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SCAN_INTERVAL") {
		t.Fatalf("error = %v, want it to name SCAN_INTERVAL", err)
	}
}

func TestUnknownTimezoneFallsBackToUTC(t *testing.T) {
	setMinimum(t)
	t.Setenv("DASHBOARD_TIMEZONE", "Mars/Olympus")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A missing tzdata must not stop the service from booting.
	if c.Location.String() != "UTC" {
		t.Errorf("Location = %v, want UTC fallback", c.Location)
	}
}
