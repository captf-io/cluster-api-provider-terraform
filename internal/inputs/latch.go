/*
Copyright 2026 The CAPTF Authors.

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

package inputs

import (
	"encoding/json"
)

// LastControlPlaneInitialized reports whether d's rendered cluster tfvars
// had control_plane_initialized true; pass the attempt record, the tfvars
// of the apply that started last. The cluster input is latched from it,
// because Cluster status is not moved by clusterctl move. Read it before
// rendering and writing the next inputs: WriteAttempt overwrites the
// tfvars. Absent, false, unparsable or a nil Record is false.
func LastControlPlaneInitialized(d *Record) bool {
	if d == nil {
		return false
	}
	var v struct {
		ControlPlaneInitialized bool `json:"control_plane_initialized"`
	}
	if err := json.Unmarshal(d.Files.TFVars, &v); err != nil {
		return false
	}
	return v.ControlPlaneInitialized
}

// LastControlPlaneEndpointNull reports whether d's rendered cluster tfvars
// had a null control_plane_endpoint; pass the record of the apply whose
// outputs are read, the applied one. The module owned the endpoint on
// that apply, so its output may be written to spec. A nil Record (no
// apply yet) or unparsable tfvars is false; an absent key counts as null.
func LastControlPlaneEndpointNull(d *Record) bool {
	if d == nil {
		return false
	}
	var v struct {
		ControlPlaneEndpoint json.RawMessage `json:"control_plane_endpoint"`
	}
	if err := json.Unmarshal(d.Files.TFVars, &v); err != nil {
		return false
	}
	return len(v.ControlPlaneEndpoint) == 0 || string(v.ControlPlaneEndpoint) == "null"
}
