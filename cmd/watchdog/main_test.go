package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWatchdogReasonPrioritizesMissingHeartbeatAndProgress(t *testing.T) {
	if reason := watchdogReason(time.Minute, fmt.Errorf("missing"), true); !strings.Contains(reason, "unavailable") {
		t.Fatalf("unexpected heartbeat error reason: %s", reason)
	}
	if reason := watchdogReason(time.Minute, nil, true); reason != "trading progress timeout" {
		t.Fatalf("unexpected progress reason: %s", reason)
	}
	if reason := watchdogReason(time.Minute, nil, false); !strings.Contains(reason, "1m0s") {
		t.Fatalf("unexpected stale heartbeat reason: %s", reason)
	}
}
