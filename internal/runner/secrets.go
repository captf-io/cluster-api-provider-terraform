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

package runner

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// bootstrapDataVar is the tfvar carrying the machine's bootstrap data, which
// is always a secret whether or not main.tf.json declares it sensitive.
const bootstrapDataVar = "bootstrap_data"

// notSecretEnv names the step environment entries that are paths, flags or
// cluster addresses, never credentials: the ones the runner or the Job sets
// itself, and PATH. Replacing them in a diagnostic would only mangle it.
var notSecretEnv = []string{
	"TF_DATA_DIR", "HOME", "TMPDIR", "TF_CLI_CONFIG_FILE", "TF_IN_AUTOMATION",
	"TF_INPUT", "KUBE_NAMESPACE", "PATH",
}

// credentialEnvMarkers are the name fragments, upper case, of environment
// entries that look like credentials whatever their length: an access key,
// a token, a password or passphrase, a private key or certificate, a
// session.
var credentialEnvMarkers = []string{
	"KEY", "SECRET", "TOKEN", "PASS", "CREDENTIAL", "PRIVATE", "AUTH", "CERT", "SESSION",
}

// minEnvSecretLen is the length from which the value of an environment
// entry whose name does not look like a credential is still redacted: an
// opaque credential under an unexpected name is rarely shorter, while a
// region, a zone, a profile or a flag ("us-east-1", "default", "true")
// usually is, and replacing those mangles diagnostics without hiding
// anything.
const minEnvSecretLen = 16

// envSecrets returns the values of env's entries that may be credentials.
// The KUBERNETES_* service-discovery variables the kubelet injects and the
// entries named in notSecretEnv never are. Of the others, an entry whose
// name contains a credentialEnvMarkers fragment (in any case) is, and any
// other entry only when its value has at least minEnvSecretLen bytes. env
// is the environment every step runs with (Prepared.Env).
func envSecrets(env []string) []string {
	var out []string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || value == "" || slices.Contains(notSecretEnv, name) || strings.HasPrefix(name, "KUBERNETES_") {
			continue
		}
		if len(value) < minEnvSecretLen && !credentialName(name) {
			continue
		}
		out = append(out, value)
	}
	return out
}

// credentialName reports whether the environment entry name looks like a
// credential's: it contains a credentialEnvMarkers fragment, in any case.
func credentialName(name string) bool {
	upper := strings.ToUpper(name)
	return slices.ContainsFunc(credentialEnvMarkers, func(m string) bool { return strings.Contains(upper, m) })
}

// minBootstrapLine is the shortest line of bootstrap data a Redactor
// removes on its own. A bootstrap script or cloud-config is mostly
// ordinary lines ("then", "fi", "runcmd:", "#cloud-config"); its secrets
// (tokens, keys, certificate lines) are longer.
const minBootstrapLine = 16

// planSecrets returns the values a plan marks sensitive as secrets whose
// lines of at least minBootstrapLine bytes are redacted on their own. A
// multi-line plan-sensitive value is most often a script or a
// cloud-config (user_data = base64decode(var.bootstrap_data)), whose
// ordinary short lines must not mangle diagnostics; a key or certificate
// has longer lines.
func planSecrets(values []string) []secret {
	return secretsWithMin(values, minBootstrapLine)
}

// varSecrets returns the secret values of the variables the rendered root in
// rootDir declares sensitive (main.tf.json), plus bootstrap_data, read from
// terraform.tfvars.json: a string variable's value, and for any other type
// every string leaf of it. bootstrap_data, sensitive or not, comes as
// given (base64) and, when that decodes to UTF-8 text, decoded as well,
// each with minBootstrapLine as the shortest of its lines redacted on its
// own. Unreadable or malformed files yield nothing: the step itself
// reports them.
func varSecrets(rootDir string) []secret {
	var vars map[string]any
	if !readJSON(filepath.Join(rootDir, "terraform.tfvars.json"), &vars) {
		return nil
	}
	var main struct {
		Variable map[string]struct {
			Sensitive bool `json:"sensitive"`
		} `json:"variable"`
	}
	var out []secret
	if readJSON(filepath.Join(rootDir, "main.tf.json"), &main) {
		for name, v := range main.Variable {
			if value, ok := vars[name]; ok && v.Sensitive && name != bootstrapDataVar {
				out = append(out, secretsOf(appendStrings(nil, value))...)
			}
		}
	}
	for _, s := range appendStrings(nil, vars[bootstrapDataVar]) {
		out = append(out, secret{value: s, minLine: minBootstrapLine})
		if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && utf8.Valid(decoded) {
			out = append(out, secret{value: string(decoded), minLine: minBootstrapLine})
		}
	}
	return out
}

// readJSON decodes the file at path into v and reports whether it could.
func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path) // #nosec G304 -- a fixed file name under the runner's own root dir
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// appendStrings appends every string leaf of the decoded JSON value v to
// out, and returns it.
func appendStrings(out []string, v any) []string {
	switch t := v.(type) {
	case string:
		out = append(out, t)
	case []any:
		for _, e := range t {
			out = appendStrings(out, e)
		}
	case map[string]any:
		for _, e := range t {
			out = appendStrings(out, e)
		}
	}
	return out
}
