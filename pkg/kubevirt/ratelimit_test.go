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

package kubevirt

import (
	"testing"

	"k8s.io/client-go/rest"
)

type fakeRateLimitConfig struct {
	qps   float32
	burst int
}

func (f fakeRateLimitConfig) GetKubeClientRateLimits() (float32, int) { return f.qps, f.burst }

func TestKubeClientRateLimits(t *testing.T) {
	tests := []struct {
		name      string
		appConfig interface{}
		wantQPS   float32
		wantBurst int
	}{
		{"no config keeps client-go defaults", nil, rest.DefaultQPS, rest.DefaultBurst},
		{"config without rate limits keeps client-go defaults", struct{}{}, rest.DefaultQPS, rest.DefaultBurst},
		{"configured values are used", fakeRateLimitConfig{qps: 100, burst: 200}, 100, 200},
		{"zero falls back per field", fakeRateLimitConfig{qps: 50, burst: 0}, 50, rest.DefaultBurst},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qps, burst := kubeClientRateLimits(tt.appConfig)
			if qps != tt.wantQPS || burst != tt.wantBurst {
				t.Errorf("got qps=%v burst=%d, want qps=%v burst=%d", qps, burst, tt.wantQPS, tt.wantBurst)
			}
		})
	}
}
