package runner

import "testing"

func TestCentralDeliveryIsolationAndBound(t *testing.T) {
	m := &Manager{}
	if !m.beginCentral("slow") || m.beginCentral("slow") || !m.beginCentral("next") || !m.beginCentral("third") || m.beginCentral("fourth") {
		t.Fatal("per-task isolation or bounded delivery failed")
	}
	m.endCentral("next")
	if !m.beginCentral("fourth") || m.beginCentral("slow") {
		t.Fatal("one stalled delivery blocked an independent result or replayed itself")
	}
}
