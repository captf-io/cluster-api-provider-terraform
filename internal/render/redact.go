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

package render

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// redactionFailed replaces tfvars that cannot be parsed: the failure mode
// is never to log them as they are.
const redactionFailed = "<redaction failed>"

// RedactedTFVars returns files.TFVars for logging, with bootstrap_data and
// every variable main.tf.json declares sensitive (user variables from a
// Secret) replaced by <redacted:len=N>. Credentials are never in tfvars:
// they are mounted files.
func RedactedTFVars(files Files) []byte {
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(files.TFVars, &vars); err != nil {
		return []byte(redactionFailed)
	}
	var main struct {
		Variable map[string]struct {
			Sensitive bool `json:"sensitive"`
		} `json:"variable"`
	}
	if err := json.Unmarshal(files.MainTF, &main); err != nil {
		return []byte(redactionFailed)
	}
	// The tfvars whose values never reach a log line.
	redact := []string{"bootstrap_data"}
	for name, v := range main.Variable {
		if v.Sensitive {
			redact = append(redact, name)
		}
	}
	for _, name := range redact {
		v, ok := vars[name]
		if !ok {
			continue
		}
		var s string
		n := len(v)
		if json.Unmarshal(v, &s) == nil {
			n = len(s)
		}
		vars[name] = json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("<redacted:len=%d>", n)))
	}
	// No HTML escaping, so the marker reads as <redacted:…> in the log.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(vars); err != nil {
		return []byte(redactionFailed)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}
