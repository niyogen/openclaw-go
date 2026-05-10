package gateway

import (
	"testing"
	"time"
)

func TestNodeCircuitOpensAfterThreshold(t *testing.T) {
	c := newNodeCircuit(nodeCircuitSettings{Threshold: 2, Cooldown: 100 * time.Millisecond})
	if err := c.before(); err != nil {
		t.Fatalf("before: %v", err)
	}
	c.recordFailure()
	c.recordFailure()
	if err := c.before(); err == nil {
		t.Fatal("expected circuit open")
	}
}

func TestNodeCircuitHalfOpenRecoversOnSuccess(t *testing.T) {
	c := newNodeCircuit(nodeCircuitSettings{Threshold: 2, Cooldown: 50 * time.Millisecond})
	c.recordFailure()
	c.recordFailure()
	if err := c.before(); err == nil {
		t.Fatal("expected open")
	}
	time.Sleep(60 * time.Millisecond)
	if err := c.before(); err != nil {
		t.Fatalf("half-open allow: %v", err)
	}
	c.recordSuccess()
	if err := c.before(); err != nil {
		t.Fatalf("closed: %v", err)
	}
}

func TestNodeCircuitHalfOpenFailsBackToOpen(t *testing.T) {
	c := newNodeCircuit(nodeCircuitSettings{Threshold: 2, Cooldown: 80 * time.Millisecond})
	c.recordFailure()
	c.recordFailure()
	time.Sleep(90 * time.Millisecond)
	if err := c.before(); err != nil {
		t.Fatal(err)
	}
	c.recordFailure()
	if err := c.before(); err == nil {
		t.Fatal("expected open again")
	}
}

func TestShouldTripNodeCircuit(t *testing.T) {
	if shouldTripNodeCircuit(&rpcError{Code: -32602, Message: "bad"}) {
		t.Fatal("param errors should not trip")
	}
	if shouldTripNodeCircuit(&rpcError{Code: -32603, Message: "bad"}) {
		t.Fatal("internal marshal should not trip")
	}
	if !shouldTripNodeCircuit(&rpcError{Code: -32000, Message: "peer"}) {
		t.Fatal("runtime errors should trip")
	}
}
