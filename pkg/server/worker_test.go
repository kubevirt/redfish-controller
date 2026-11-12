/*
 * This file is part of the KubeVirt Redfish project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2025 KubeVirt Redfish project and its authors.
 *
 */

package server

import (
	"testing"
	"time"
)

// Stopping a worker must return once its work loop has exited. Server
// shutdown waits on this, so a Stop that never returns keeps the process
// alive until the kubelet kills it.
func TestWorkerStopReturns(t *testing.T) {
	tm := NewTaskManager(1, nil)

	stopped := make(chan struct{})
	go func() {
		tm.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("TaskManager.Stop did not return within 5s: Worker.Stop waits on a WaitGroup that the work loop never releases")
	}
}
