/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

package render

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactedTFVars checks that RedactedTFVars replaces bootstrap_data and
// declared-sensitive variables while leaving other values and unparsable
// input passed through as redactionFailed.
func TestRedactedTFVars(t *testing.T) {
	t.Parallel()
	secret := "I2Nsb3VkLWNvbmZpZwo=" // base64 of the bootstrap payload
	mainTF := []byte(`{"variable":{"machine_name":{"type":"string"}}}`)
	got := string(RedactedTFVars(Files{MainTF: mainTF, TFVars: []byte(`{"bootstrap_data":"` + secret + `","machine_name":"m1","captf_cluster_outputs":{"vpc":"v"}}`)}))
	if strings.Contains(got, secret) {
		t.Fatalf("bootstrap_data leaked: %s", got)
	}
	var vars map[string]any
	if err := json.Unmarshal([]byte(got), &vars); err != nil {
		t.Fatalf("not JSON: %s", got)
	}
	if vars["bootstrap_data"] != "<redacted:len=20>" || vars["machine_name"] != "m1" || vars["captf_cluster_outputs"] == nil {
		t.Errorf("redacted = %s", got)
	}
	// Tfvars without bootstrap data (the cluster role) pass through.
	if got := string(RedactedTFVars(Files{MainTF: mainTF, TFVars: []byte(`{"a":1}`)})); got != `{"a":1}` {
		t.Errorf("cluster tfvars = %s", got)
	}
	// Unparsable tfvars are never logged as they are.
	if got := string(RedactedTFVars(Files{MainTF: mainTF, TFVars: []byte(`{"bootstrap_data":` + secret)})); got != redactionFailed {
		t.Errorf("unparsable = %s", got)
	}
}
