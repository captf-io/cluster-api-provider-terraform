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

package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// validateSource checks src's image reference syntax, with fldPath rooted
// at src's parent field. Image content and registry are deliberately not
// policed here. It returns the field errors found, or nil when src is
// valid.
func validateSource(fldPath *field.Path, src *infrav1.Source) field.ErrorList {
	imagePath := fldPath.Child("image")
	if src.Image == "" {
		return field.ErrorList{field.Required(imagePath, "an image reference is required")}
	}
	if _, err := reference.ParseNormalizedNamed(src.Image); err != nil {
		return field.ErrorList{field.Invalid(imagePath, src.Image, fmt.Sprintf("not a valid image reference: %v", err))}
	}
	return nil
}

// validateJobPolicy checks a jobs policy wherever one appears (a cluster, its
// defaults, a machine, a template). The main container holds cloud
// credentials, so neither its security context nor the pod's may weaken the
// hardened defaults (see validateContainerSecurityContext); and a lock wait that fills the whole deadline would fail as a
// deadline, not as a lock wait; a lone field is compared with the other's
// built-in default. The cross-field check covers one policy only: a
// machine's field-wise merge with the cluster's defaults is not visible
// here. fldPath is rooted at p's parent field. It returns the field
// errors found, or nil when p is nil or valid.
func validateJobPolicy(fldPath *field.Path, p *infrav1.JobPolicy) field.ErrorList {
	if p == nil {
		return nil
	}
	var errs field.ErrorList
	errs = append(errs, validateContainerSecurityContext(fldPath.Child("securityContext"), p.SecurityContext)...)
	errs = append(errs, validatePodSecurityContext(fldPath.Child("podSecurityContext"), p.PodSecurityContext)...)
	for i, e := range p.Env {
		if jobs.ReservedEnv(e.Name) {
			errs = append(errs, field.Forbidden(fldPath.Child("env").Index(i).Child("name"),
				"the name is reserved: the runner owns TF_*, KUBE_* and KUBERNETES_* variables, HOME, TMPDIR and CHECKPOINT_DISABLE"))
		}
	}
	lock, deadline := p.LockTimeoutSeconds, p.ActiveDeadlineSeconds
	switch {
	case lock != nil && deadline != 0:
		if int64(*lock) >= deadline {
			errs = append(errs, field.Invalid(fldPath.Child("lockTimeoutSeconds"), *lock,
				fmt.Sprintf("must be less than activeDeadlineSeconds (%d), or a Job could spend its whole deadline waiting for the state lock", deadline)))
		}
	case deadline != 0 && deadline <= int64(jobs.DefaultLockTimeoutSeconds):
		errs = append(errs, field.Invalid(fldPath.Child("activeDeadlineSeconds"), deadline,
			fmt.Sprintf("must be greater than the default lockTimeoutSeconds (%d) while lockTimeoutSeconds is unset, or a Job could spend its whole deadline waiting for the state lock; raise it or set a lower lockTimeoutSeconds", jobs.DefaultLockTimeoutSeconds)))
	case lock != nil && deadline == 0 && int64(*lock) >= jobs.DefaultActiveDeadlineSeconds:
		errs = append(errs, field.Invalid(fldPath.Child("lockTimeoutSeconds"), *lock,
			fmt.Sprintf("must be less than the default activeDeadlineSeconds (%d) while activeDeadlineSeconds is unset, or a Job could spend its whole deadline waiting for the state lock; lower it or set a larger activeDeadlineSeconds", jobs.DefaultActiveDeadlineSeconds)))
	}
	return errs
}

// validateJobPolicyChange is validateJobPolicy for a policy that may already
// be stored: it checks cur only when it differs from old. A policy stored
// before a rule existed would otherwise reject every later update of its
// object, including the controller's finalizer and annotation patches and a
// finalizer removal. fldPath is rooted at the policy's parent field; create
// passes a nil old, so cur is always checked. It returns the field errors found, or nil when cur is unchanged or valid.
func validateJobPolicyChange(fldPath *field.Path, old, cur *infrav1.JobPolicy) field.ErrorList {
	if equality.Semantic.DeepEqual(old, cur) {
		return nil
	}
	return validateJobPolicy(fldPath, cur)
}

// priorSpec returns the spec to compare a new spec against for the jobs
// policy checks of an update: old normally, but cur itself when the object
// is deleting, so a policy never blocks an update of a deleting object
// (its finalizers must stay removable).
func priorSpec[S any](old, cur *S, deleting bool) *S {
	if deleting {
		return cur
	}
	return old
}

// validateContainerSecurityContext checks the Job container's security
// context sc at scPath: the container holds cloud credentials, so it may not
// be privileged, escalate privileges, add capabilities, run as root, have a
// writable root filesystem, an unconfined seccomp or AppArmor profile, a
// SELinux type outside the baseline set (or a SELinux user or role), an
// unmasked /proc or a Windows host process. It returns the field errors found, or
// nil when sc is nil or valid.
func validateContainerSecurityContext(scPath *field.Path, sc *corev1.SecurityContext) field.ErrorList {
	if sc == nil {
		return nil
	}
	const holds = "the Job container holds cloud credentials and "
	var errs field.ErrorList
	if sc.Privileged != nil && *sc.Privileged {
		errs = append(errs, field.Forbidden(scPath.Child("privileged"), holds+"may not be privileged"))
	}
	if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation {
		errs = append(errs, field.Forbidden(scPath.Child("allowPrivilegeEscalation"), holds+"may not escalate privileges"))
	}
	if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
		errs = append(errs, field.Forbidden(scPath.Child("capabilities", "add"), holds+"may not add capabilities"))
	}
	if sc.ReadOnlyRootFilesystem != nil && !*sc.ReadOnlyRootFilesystem {
		errs = append(errs, field.Forbidden(scPath.Child("readOnlyRootFilesystem"), holds+"needs a read-only root filesystem"))
	}
	if sc.ProcMount != nil && *sc.ProcMount == corev1.UnmaskedProcMount {
		errs = append(errs, field.Forbidden(scPath.Child("procMount"), holds+"may not unmask /proc"))
	}
	errs = append(errs, validateIdentity(scPath, "container", sc.RunAsNonRoot, sc.RunAsUser, sc.SeccompProfile, sc.AppArmorProfile, sc.SELinuxOptions, sc.WindowsOptions)...)
	return errs
}

// validatePodSecurityContext checks the Job pod's security context sc at
// scPath: the same seccomp, AppArmor, SELinux, root and host process rules as the
// container's, since a pod-level setting is inherited by it, and only
// sysctls from the baseline safe set. It returns the field errors
// found, or nil when sc is nil or valid.
func validatePodSecurityContext(scPath *field.Path, sc *corev1.PodSecurityContext) field.ErrorList {
	if sc == nil {
		return nil
	}
	errs := validateIdentity(scPath, "pod", sc.RunAsNonRoot, sc.RunAsUser, sc.SeccompProfile, sc.AppArmorProfile, sc.SELinuxOptions, sc.WindowsOptions)
	for i, sysctl := range sc.Sysctls {
		if !safeSysctls.Has(sysctl.Name) {
			errs = append(errs, field.Forbidden(scPath.Child("sysctls").Index(i).Child("name"),
				"the Job pod holds cloud credentials and may set only the sysctls the Pod Security Standards baseline allows"))
		}
	}
	return errs
}

// safeSysctls is the sysctl allowlist of the Pod Security Standards
// baseline profile, which equals the kubelet's safe set. Copied from
// k8s.io/pod-security-admission policy/check_sysctls.go (Kubernetes 1.36)
// rather than imported, to avoid the dependency; refresh it when the
// Kubernetes minor version is bumped.
var safeSysctls = sets.New(
	"kernel.shm_rmid_forced",
	"net.ipv4.ip_local_port_range",
	"net.ipv4.tcp_syncookies",
	"net.ipv4.ping_group_range",
	"net.ipv4.ip_unprivileged_port_start",
	"net.ipv4.ip_local_reserved_ports",
	"net.ipv4.tcp_keepalive_time",
	"net.ipv4.tcp_fin_timeout",
	"net.ipv4.tcp_keepalive_intvl",
	"net.ipv4.tcp_keepalive_probes",
	"net.ipv4.tcp_rmem",
	"net.ipv4.tcp_wmem",
)

// baselineSELinuxTypes is the SELinux type allowlist of the Pod Security
// Standards baseline profile (k8s.io/pod-security-admission
// policy/check_seLinuxOptions.go, Kubernetes 1.36); the empty type is the
// runtime default.
var baselineSELinuxTypes = sets.New("", "container_t", "container_init_t", "container_kvm_t", "container_engine_t")

// validateIdentity checks the fields a pod and a container security context
// share, rooted at scPath: nonRoot and user (running as root), seccomp and
// apparmor (an unconfined profile), selinux (a type outside the baseline
// set, or a user or role) and win (a Windows host process). scope names the
// context in messages ("pod" or "container"). It returns the field errors
// found.
func validateIdentity(scPath *field.Path, scope string, nonRoot *bool, user *int64, seccomp *corev1.SeccompProfile, apparmor *corev1.AppArmorProfile, selinux *corev1.SELinuxOptions, win *corev1.WindowsSecurityContextOptions) field.ErrorList {
	holds := "the Job " + scope + " holds cloud credentials and "
	var errs field.ErrorList
	if nonRoot != nil && !*nonRoot {
		errs = append(errs, field.Forbidden(scPath.Child("runAsNonRoot"), holds+"may not opt out of runAsNonRoot"))
	}
	if user != nil && *user == 0 {
		errs = append(errs, field.Forbidden(scPath.Child("runAsUser"), holds+"may not run as UID 0"))
	}
	if seccomp != nil && seccomp.Type == corev1.SeccompProfileTypeUnconfined {
		errs = append(errs, field.Forbidden(scPath.Child("seccompProfile", "type"), holds+"may not run with an Unconfined seccomp profile"))
	}
	if apparmor != nil && apparmor.Type == corev1.AppArmorProfileTypeUnconfined {
		errs = append(errs, field.Forbidden(scPath.Child("appArmorProfile", "type"), holds+"may not run with an Unconfined AppArmor profile"))
	}
	if selinux != nil {
		if !baselineSELinuxTypes.Has(selinux.Type) {
			errs = append(errs, field.Forbidden(scPath.Child("seLinuxOptions", "type"), holds+"may set only a SELinux type the Pod Security Standards baseline allows"))
		}
		if selinux.User != "" {
			errs = append(errs, field.Forbidden(scPath.Child("seLinuxOptions", "user"), holds+"may not set a SELinux user"))
		}
		if selinux.Role != "" {
			errs = append(errs, field.Forbidden(scPath.Child("seLinuxOptions", "role"), holds+"may not set a SELinux role"))
		}
	}
	if win != nil && win.HostProcess != nil && *win.HostProcess {
		errs = append(errs, field.Forbidden(scPath.Child("windowsOptions", "hostProcess"), holds+"may not run as a Windows host process"))
	}
	return errs
}

// validateVariables checks spec.variables and spec.variablesFrom for role:
// the inline variables are a JSON object of at most contract.MaxVariables
// keys, each a Terraform identifier that is neither a captf_ name, a
// contract input of role nor a module meta-argument; each source in from
// names exactly one ConfigMap or Secret. specPath is rooted at the spec
// holding inline and from. Keys of referenced sources are checked at
// reconcile (VariablesInvalid). No error carries a value. It returns the
// field errors found, or nil when inline and from are valid.
func validateVariables(specPath *field.Path, role contract.Role, inline runtime.RawExtension, from []infrav1.VariablesSource) field.ErrorList {
	var errs field.ErrorList
	varsPath := specPath.Child("variables")
	if len(inline.Raw) > 0 {
		vars, err := contract.ParseVariables(inline.Raw)
		switch {
		case err != nil:
			errs = append(errs, field.Invalid(varsPath, "(value omitted)", "must be a JSON object of module variables"))
		case len(vars) > contract.MaxVariables:
			errs = append(errs, field.TooMany(varsPath, len(vars), contract.MaxVariables))
		}
		for _, name := range slices.Sorted(maps.Keys(vars)) {
			if err := contract.ValidateVariableName(role, name); err != nil {
				errs = append(errs, field.Invalid(varsPath.Key(name), name, strings.TrimPrefix(err.Error(), "contract: ")))
			}
		}
	}
	for i, src := range from {
		p := specPath.Child("variablesFrom").Index(i)
		if (src.ConfigMapRef.Name == "") == (src.SecretRef.Name == "") {
			errs = append(errs, field.Invalid(p, "", "set exactly one of configMapRef and secretRef"))
		}
	}
	return errs
}

// equalVariables compares the inline variables a and b by value, so a
// re-serialization (key order, whitespace) is no change: on an immutable
// machine a false change would be rejected, and KCP would roll the machine
// instead. It reports whether a and b are equal.
func equalVariables(a, b runtime.RawExtension) bool {
	if bytes.Equal(a.Raw, b.Raw) {
		return true
	}
	var va, vb any
	if json.Unmarshal(a.Raw, &va) != nil || json.Unmarshal(b.Raw, &vb) != nil {
		return false
	}
	return equality.Semantic.DeepEqual(va, vb)
}

// invalid wraps errs into the 422 Invalid status for the object of the
// given kind and name. A denial is logged at V(1) through ctx's logger with
// the requesting user, the object's kind and name, and each violation's
// field path and type; the detail and the rejected value are left out so no
// object content reaches the log. It returns that error, or nil when errs
// is empty.
func invalid(ctx context.Context, kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	if log := klog.FromContext(ctx).V(1); log.Enabled() {
		causes := make([]string, 0, len(errs))
		for _, e := range errs {
			causes = append(causes, e.Field+" ("+string(e.Type)+")")
		}
		log.Info("Admission denied", "user", requestUser(ctx), "kind", kind, "name", name, "violations", causes)
	}
	return apierrors.NewInvalid(infrav1.GroupVersion.WithKind(kind).GroupKind(), name, errs)
}

// validateWorkloadSpec runs the checks a machine and a machine pool spec
// share: the source, the jobs policy and the variables for role. specPath is
// rooted at spec's parent field; spec is the workspace part being validated;
// old is the stored workspace on an update (the jobs policy is checked only
// when it changed from there), or nil on create. It returns the field
// errors found, or nil when spec is valid.
func validateWorkloadSpec(specPath *field.Path, role contract.Role, spec, old *infrav1.WorkspaceSpec) field.ErrorList {
	var oldJobs *infrav1.JobPolicy
	if old != nil {
		oldJobs = old.Jobs
	}
	errs := validateSource(specPath.Child("source"), &spec.Source)
	errs = append(errs, validateJobPolicyChange(specPath.Child("jobs"), oldJobs, spec.Jobs)...)
	return append(errs, validateVariables(specPath, role, spec.Variables, spec.VariablesFrom)...)
}

// requestUser returns the username of the admission request carried by ctx,
// or "" when ctx carries none (a call outside the webhook server).
func requestUser(ctx context.Context) string {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return ""
	}
	return req.UserInfo.Username
}

// immutable is the field.Forbidden cause for a changed immutable field at
// fldPath of an object of the given kind. It returns that field.Error.
func immutable(fldPath *field.Path, kind string) *field.Error {
	return field.Forbidden(fldPath, fmt.Sprintf("%s %s is immutable; create a new %s instead", kind, fldPath.String(), kind))
}

// SchemaLookup is what the webhooks need of imageinspect.SchemaCache: the
// variables schema an image is already known to declare. It never
// contacts a registry.
type SchemaLookup interface {
	// Cached returns the schema of ref's image (nil when it declares none)
	// and true, or false when the image has not been inspected.
	Cached(ref string) (*varschema.Schema, bool)
}

// schemaErrors checks ws's inline variables against the variables schema
// lookup already holds for ws's image; it returns nil when lookup is nil,
// the image is unknown or declares no schema, there are no inline
// variables, or old (the stored spec on an update, nil on create) has the
// same image and variables. variablesFrom sources are not readable at
// admission, so a required variable is not reported here: the controller
// checks the merged variables before the Job. specPath is rooted at the
// spec holding ws. No error carries a value. It returns the field errors
// found.
func schemaErrors(lookup SchemaLookup, specPath *field.Path, ws, old *infrav1.WorkspaceSpec) field.ErrorList {
	if lookup == nil || len(ws.Variables.Raw) == 0 {
		return nil
	}
	if old != nil && old.Source.Image == ws.Source.Image && equalVariables(old.Variables, ws.Variables) {
		return nil
	}
	schema, ok := lookup.Cached(ws.Source.Image)
	if !ok || schema == nil {
		return nil
	}
	raw, err := contract.ParseVariables(ws.Variables.Raw)
	if err != nil {
		return nil
	}
	vars := make(map[string]varschema.Value, len(raw))
	for name, v := range raw {
		vars[name] = varschema.Value{JSON: v}
	}
	var errs field.ErrorList
	for _, msg := range schema.ValidatePartial(vars) {
		errs = append(errs, field.Invalid(specPath.Child("variables"), "(value omitted)", "the module image's "+varschema.Label+" rejects it: "+msg))
	}
	return errs
}
