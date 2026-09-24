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
	"strconv"
	"time"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultOSDSlowOpThreshold = time.Second

// tracingConfig translates the tracing settings into Ceph config options. It returns nil when the
// settings are not given, so that Rook leaves the options alone.
func tracingConfig(t *cephv1.TracingSpec) (map[string]map[string]string, error) {
	if t == nil {
		return nil, nil
	}
	if !t.Enabled {
		// a zero threshold turns the traces off; the rest can stay as it is
		return map[string]map[string]string{
			"osd":    {"osd_op_trace_slow_threshold": "0"},
			"client": {"rgw_trace_slow_threshold": "0"},
		}, nil
	}
	if t.Endpoint == "" {
		return nil, errors.New("tracing is enabled but no endpoint is set")
	}

	osdThreshold := seconds(t.OSDSlowOpThreshold, defaultOSDSlowOpThreshold)
	osd := map[string]string{
		"osd_op_trace_slow_threshold":       osdThreshold,
		"osd_op_trace_slow_require_context": strconv.FormatBool(t.OnlyClientRequests),
	}
	// RGW defaults to the OSDs' threshold: with a higher one, the OSDs trace ops of
	// requests that RGW does not, and those appear without the request above them
	rgwThreshold := osdThreshold
	if t.RGWSlowRequestThreshold != nil {
		rgwThreshold = seconds(t.RGWSlowRequestThreshold, 0)
	}
	rgw := map[string]string{
		"rgw_trace_slow_threshold": rgwThreshold,
	}
	if t.MaxTracesPerSecond != nil {
		n := strconv.FormatUint(uint64(*t.MaxTracesPerSecond), 10)
		osd["osd_op_trace_max_per_sec"] = n
		rgw["rgw_trace_max_per_sec"] = n
	}
	return map[string]map[string]string{
		"global": {
			"trace_exporter":      "otlp",
			"trace_otlp_endpoint": t.Endpoint,
		},
		"osd":    osd,
		"client": rgw,
	}, nil
}

func seconds(d *metav1.Duration, def time.Duration) string {
	v := def
	if d != nil {
		v = d.Duration
	}
	return strconv.FormatFloat(v.Seconds(), 'f', -1, 64)
}
