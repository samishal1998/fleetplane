package compute

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

func TestParseSpecValid(t *testing.T) {
	raw := json.RawMessage(`{"serverType":"cpx31","image":"snapshot:ci=1","location":"fsn1",
		"readiness":{"tcp":{"port":22,"timeout":"2s"},"budget":"3m"}}`)
	s, err := ParseSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Readiness.TCP.Port != 22 || s.Readiness.TCP.Timeout.Std() != 2*time.Second {
		t.Fatalf("readiness parsed wrong: %+v", s.Readiness)
	}
	if s.Readiness.Budget.Std() != 3*time.Minute {
		t.Fatalf("budget = %v", s.Readiness.Budget.Std())
	}
}

func TestParseSpecRejectsMissingFields(t *testing.T) {
	for _, raw := range []string{`{}`, `{"serverType":"cpx31"}`, `{"image":"x"}`} {
		if _, err := ParseSpec(json.RawMessage(raw)); err == nil {
			t.Errorf("ParseSpec(%s) accepted", raw)
		}
	}
}

func TestHTTPProbeRejectedInV1(t *testing.T) {
	raw := json.RawMessage(`{"serverType":"a","image":"b","readiness":{"http":{"port":80,"path":"/"}}}`)
	_, err := ParseSpec(raw)
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("err = %v, want http-not-supported (plan R16)", err)
	}
}

func TestDurationJSONRoundTrip(t *testing.T) {
	b, err := json.Marshal(Duration(90 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var d Duration
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Std() != 90*time.Second {
		t.Fatalf("round trip = %v", d.Std())
	}
	if err := json.Unmarshal([]byte(`5`), &d); err == nil {
		t.Fatal("bare number accepted as duration")
	}
}

func TestProbeOnceTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	addrs := []provider.Address{{Network: "public-v4", Addr: "127.0.0.1"}}
	spec := &ReadinessSpec{TCP: &TCPProbe{Port: port}}
	if err := ProbeOnce(context.Background(), addrs, spec); err != nil {
		t.Fatalf("probe against live listener failed: %v", err)
	}

	// Closed port fails.
	_ = ln.Close()
	if err := ProbeOnce(context.Background(), addrs, spec); err == nil {
		t.Fatal("probe against closed port succeeded")
	}

	// No probe configured = pass (provider readiness suffices).
	if err := ProbeOnce(context.Background(), addrs, nil); err != nil {
		t.Fatal(err)
	}
}
