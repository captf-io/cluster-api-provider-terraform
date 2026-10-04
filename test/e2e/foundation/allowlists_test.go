//go:build e2e

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

package foundation

import (
	"regexp"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/health"
)

// Every allowlist entry carries the reason it is benign; test/README.md
// repeats them. Keep the lists short: a new entry needs evidence that the
// line or event is expected on a healthy cluster.

// allowed is one allowlist entry.
type allowed struct {
	// pattern is the regular expression a line or event must match.
	pattern string
	// reason says why a match is benign.
	reason string
}

// benignErrorLines are the klog E-level lines cert-manager, CAPI and
// kube-system print on a healthy kind cluster. Those components fail the
// suite only on panic or fatal lines; any other E line is reported
// without failing, and these are left out of that report.
var benignErrorLines = []allowed{
	{
		pattern: `unable to fetch associated secret`,
		reason:  "cert-manager cainjector, at install: a Certificate's Secret is not issued yet; cainjector retries and injects the CA once it exists",
	},
	{
		pattern: `re-queuing item due to error processing`,
		reason:  "cert-manager controller, at install: a transient conflict while the first Certificates are issued; the item is retried and becomes Ready",
	},
	{
		pattern: `unable to fetch certificate that owns the secret`,
		reason:  "cert-manager cainjector, after stage 4: deleting the throwaway namespace removes its Certificate before its Secret, and the indexer logs the orphan once",
	},
	{
		pattern: `nodePortAddresses is unset`,
		reason:  "kube-proxy: kind leaves nodePortAddresses unset; the line is advice, not a failure",
	},
	{
		pattern: `The manifest file is empty, ignoring`,
		reason:  "kube-apiserver, kube-controller-manager and kube-scheduler at startup: kubeadm passes an empty optional manifest file",
	},
	{
		pattern: `system:kube-(scheduler|controller-manager)\S* cannot (get|list|watch)`,
		reason:  "kube-scheduler and kube-controller-manager in their first seconds: their informers start before the API server has created the bootstrap RBAC policy, and they retry",
	},
	{
		pattern: `is not as new as written version`,
		reason:  "kube-controller-manager: its informer cache briefly lags a write it made (consistent-read check); the sync is retried",
	},
}

// warningEventAllow are the Warning events (matched against "reason:
// message") the stability window tolerates in the provider namespaces.
// None are known: a healthy cluster emits no Warning event there once the
// providers are installed.
var warningEventAllow = []allowed{}

// compile returns the compiled patterns of list. It panics on an invalid
// pattern, which is a bug in this file.
func compile(list []allowed) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(list))
	for _, a := range list {
		out = append(out, regexp.MustCompile(a.pattern))
	}
	return out
}

// captfLogRules returns the rules CAPTF's manager log is held to:
// health.DefaultFatal (panic, fatal error, klog E and F lines) with no
// allowlist.
func captfLogRules() health.Rules {
	return health.Rules{Fatal: health.DefaultFatal()}
}

// componentLogRules returns the rules that fail the other components'
// logs: Go panics, fatal errors and klog F lines. E lines do not fail
// them; see errorLineRules.
func componentLogRules() health.Rules {
	return health.Rules{Fatal: []*regexp.Regexp{
		regexp.MustCompile(`panic:`),
		regexp.MustCompile(`fatal error:`),
		regexp.MustCompile(`^F\d{4} `),
	}}
}

// errorLineRules returns the rules of the non-failing E-line report on
// the other components: klog E lines, minus benignErrorLines.
func errorLineRules() health.Rules {
	return health.Rules{
		Fatal: []*regexp.Regexp{regexp.MustCompile(`^E\d{4} `)},
		Allow: compile(benignErrorLines),
	}
}
