package kubevirt

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/kubevirt/redfish-controller/pkg/logger"
)

// captureLog routes the package logger to a buffer at DEBUG level for the
// duration of the test, so a log line that reintroduces the full URL fails it.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	logger.Init("DEBUG")
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(log.Writer())
		logger.Init("INFO")
	})
	return &buf
}

const (
	redactedISOURL = "http://example.com/images/abc/full.iso"
	secretISOURL   = redactedISOURL + "?api_key=SECRET-TOKEN"
)

func TestInsertVirtualMedia_DoesNotLogTheImageToken(t *testing.T) {
	client, _, _ := setupInsertVirtualMediaTest(t, false)
	buf := captureLog(t)

	if err := client.InsertVirtualMedia("test-ns", "test-vm", "cdrom0", secretISOURL); err != nil {
		t.Fatalf("InsertVirtualMedia failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "SECRET-TOKEN") {
		t.Errorf("the image token was logged:\n%s", linesContaining(out, "SECRET-TOKEN"))
	}
	if !strings.Contains(out, redactedISOURL) {
		t.Errorf("the redacted image URL was not logged at all:\n%s", out)
	}
}

func TestInsertVirtualMedia_UnparsableURLIsNotLogged(t *testing.T) {
	client, _, _ := setupInsertVirtualMediaTest(t, false)
	buf := captureLog(t)

	if err := client.InsertVirtualMedia("test-ns", "test-vm", "cdrom0", "://SECRET-TOKEN"); err == nil {
		t.Fatal("expected an error for an unparsable URL")
	}
	if out := buf.String(); strings.Contains(out, "SECRET-TOKEN") {
		t.Errorf("the unparsable URL was logged:\n%s", linesContaining(out, "SECRET-TOKEN"))
	}
}

func linesContaining(s, needle string) string {
	var hits []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			hits = append(hits, line)
		}
	}
	return strings.Join(hits, "\n")
}
