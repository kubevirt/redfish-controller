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

import "testing"

// With every worker idle, consecutive jobs must be spread across the pool.
// A dispatcher that always returns the first worker with queue capacity
// serializes every job onto worker 1 while the others stay idle.
func TestGetAvailableWorkerRoundRobin(t *testing.T) {
	const workers = 4
	tm := NewTaskManager(workers, nil)
	defer tm.cancel()

	picks := make(map[int]int)
	for i := 0; i < 2*workers; i++ {
		w := tm.getAvailableWorker()
		if w == nil {
			t.Fatalf("pick %d: no worker available although all queues are empty", i)
		}
		picks[w.ID]++
	}

	for id := 1; id <= workers; id++ {
		if picks[id] != 2 {
			t.Errorf("worker %d was picked %d times, want 2 (picks per worker: %v)", id, picks[id], picks)
		}
	}
}
