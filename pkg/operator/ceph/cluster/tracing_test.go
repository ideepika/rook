/*
Copyright 2026 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"testing"
	"time"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTracingConfig(t *testing.T) {
	t.Run("not set leaves the options alone", func(t *testing.T) {
		cfg, err := tracingConfig(nil)
		assert.NoError(t, err)
		assert.Nil(t, cfg)
	})

	t.Run("disabled turns the traces off", func(t *testing.T) {
		cfg, err := tracingConfig(&cephv1.TracingSpec{Endpoint: "http://jaeger:4318/v1/traces"})
		assert.NoError(t, err)
		assert.Equal(t, map[string]map[string]string{
			"osd":    {"osd_op_trace_slow_threshold": "0"},
			"client": {"rgw_trace_slow_threshold": "0"},
		}, cfg)
	})

	t.Run("enabled needs an endpoint", func(t *testing.T) {
		_, err := tracingConfig(&cephv1.TracingSpec{Enabled: true})
		assert.Error(t, err)
	})

	t.Run("defaults", func(t *testing.T) {
		cfg, err := tracingConfig(&cephv1.TracingSpec{Enabled: true, Endpoint: "http://jaeger:4318/v1/traces"})
		assert.NoError(t, err)
		assert.Equal(t, map[string]map[string]string{
			"global": {"trace_exporter": "otlp", "trace_otlp_endpoint": "http://jaeger:4318/v1/traces"},
			"osd":    {"osd_op_trace_slow_threshold": "1", "osd_op_trace_slow_require_context": "false"},
			"client": {"rgw_trace_slow_threshold": "1"},
		}, cfg)
	})

	t.Run("rgw follows the osd threshold", func(t *testing.T) {
		cfg, err := tracingConfig(&cephv1.TracingSpec{
			Enabled:            true,
			Endpoint:           "http://jaeger:4318/v1/traces",
			OSDSlowOpThreshold: &metav1.Duration{Duration: 300 * time.Millisecond},
		})
		assert.NoError(t, err)
		assert.Equal(t, "0.3", cfg["osd"]["osd_op_trace_slow_threshold"])
		assert.Equal(t, "0.3", cfg["client"]["rgw_trace_slow_threshold"])
	})

	t.Run("all settings", func(t *testing.T) {
		max := uint32(50)
		cfg, err := tracingConfig(&cephv1.TracingSpec{
			Enabled:                 true,
			Endpoint:                "https://tempo.monitoring.svc:4318/v1/traces",
			OSDSlowOpThreshold:      &metav1.Duration{Duration: 250 * time.Millisecond},
			RGWSlowRequestThreshold: &metav1.Duration{Duration: 2 * time.Second},
			OnlyClientRequests:      true,
			MaxTracesPerSecond:      &max,
		})
		assert.NoError(t, err)
		assert.Equal(t, map[string]map[string]string{
			"global": {"trace_exporter": "otlp", "trace_otlp_endpoint": "https://tempo.monitoring.svc:4318/v1/traces"},
			"osd": {
				"osd_op_trace_slow_threshold":       "0.25",
				"osd_op_trace_slow_require_context": "true",
				"osd_op_trace_max_per_sec":          "50",
			},
			"client": {"rgw_trace_slow_threshold": "2", "rgw_trace_max_per_sec": "50"},
		}, cfg)
	})
}
