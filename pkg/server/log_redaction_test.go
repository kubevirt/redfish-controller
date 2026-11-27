package server

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/kubevirt/redfish-controller/pkg/logger"
)

func TestTaskManager_DoesNotLogTheImageToken(t *testing.T) {
	var buf bytes.Buffer
	logger.Init("DEBUG")
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(log.Writer())
		logger.Init("INFO")
	})

	tm := NewTaskManager(0, nil)
	t.Cleanup(tm.Stop)

	tm.CreateTask("insert", "ns", "vm", "cdrom0", "http://example.com/full.iso?api_key=SECRET-TOKEN")

	if out := buf.String(); strings.Contains(out, "SECRET-TOKEN") {
		t.Errorf("the image token was logged:\n%s", out)
	}
}
