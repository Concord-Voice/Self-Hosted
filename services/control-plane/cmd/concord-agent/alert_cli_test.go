package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/hostagent"
)

func TestAlertCLIUsageAndRootRequirement(t *testing.T) {
	for _, args := range [][]string{nil, {"send"}, {"test", "extra"}} {
		var out, errOut bytes.Buffer
		if code := runAlertCLI(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage: concord-agent alert test") {
			t.Fatalf("args %q: code %d, stderr %q", args, code, errOut.String())
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("the non-root refusal needs a non-root test process")
	}
	var out, errOut bytes.Buffer
	if code := runAlertCLI([]string{"test"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "run as root") || out.Len() != 0 {
		t.Fatalf("non-root alert test: code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestConfigureAlertsWarnsWhenNoDestinationExists(t *testing.T) {
	if _, err := os.Lstat(hostagent.AlertConfigPath); err == nil {
		t.Skip("this host has an alert destination configured")
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	configureAlerts(context.Background(), hostagent.NewServer(nil))
	if !strings.Contains(logs.String(), "host actions are not alerted out of band") || strings.Count(logs.String(), "\n") != 1 {
		t.Fatalf("want one startup warning, got %q", logs.String())
	}
}
