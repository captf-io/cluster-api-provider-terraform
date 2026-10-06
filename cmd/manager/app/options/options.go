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

package options

import (
	"crypto/tls"
	goflag "flag"
	"fmt"
	"time"

	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/featuregate"
	logsv1 "k8s.io/component-base/logs/api/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/flags"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/captf-io/cluster-api-provider-terraform/internal/feature"
	captfmanager "github.com/captf-io/cluster-api-provider-terraform/internal/manager"
)

const (
	// LeaderElectionID is the Lease name, in the manager namespace.
	LeaderElectionID = "controller-leader-election-captf"

	// GracefulShutdownTimeout is how long the manager gives its runnables to
	// stop after SIGTERM before it returns. The Deployment's
	// terminationGracePeriodSeconds is 30s, of which the container's
	// preStop sleep takes 5s, so this leaves 5s of slack before the kubelet
	// sends SIGKILL. controller-runtime's default, 30s, would be cut short.
	GracefulShutdownTimeout = 20 * time.Second

	// ManagerImageEnv names the environment variable, set by the
	// Deployment, that carries the manager's own image reference; it is the
	// default of --runner-image.
	ManagerImageEnv = "CAPTF_MANAGER_IMAGE"

	// PodNamespaceEnv names the environment variable, set by the Deployment
	// through the downward API, that carries the manager's namespace.
	PodNamespaceEnv = "POD_NAMESPACE"

	// ServiceAccountEnv names the environment variable, set by the
	// Deployment through the downward API, that carries the manager's
	// ServiceAccount name.
	ServiceAccountEnv = "SERVICE_ACCOUNT_NAME"

	// DefaultWebhookCertDir is where config/default mounts the serving
	// certificate.
	DefaultWebhookCertDir = "/tmp/k8s-webhook-server/serving-certs/"
)

// Options are the parsed manager flags.
type Options struct {
	// Namespace restricts the cache to one namespace; empty watches all.
	Namespace string
	// WatchFilter is the cluster.x-k8s.io/watch-filter label value.
	WatchFilter string

	LeaderElect                 bool
	LeaderElectionLeaseDuration time.Duration
	LeaderElectionRenewDeadline time.Duration
	LeaderElectionRetryPeriod   time.Duration

	SyncPeriod time.Duration

	TerraformClusterConcurrency         int
	TerraformMachineConcurrency         int
	TerraformMachineTemplateConcurrency int
	TerraformMachinePoolConcurrency     int

	WebhookPort     int
	WebhookCertDir  string
	WebhookCertName string
	WebhookKeyName  string

	HealthAddr      string
	ProfilerAddress string

	// RunnerImage is the image of the init container that injects the
	// runner binary into Jobs; it defaults to the manager's own image.
	RunnerImage string
	// ManagerUser is the manager's ServiceAccount username
	// (system:serviceaccount:<namespace>:<name>), built by Complete from
	// $POD_NAMESPACE and $SERVICE_ACCOUNT_NAME; empty when either is unset.
	// The TerraformMachine webhook lets only this user set providerID.
	ManagerUser string
	// RunnerEvents makes Job runners report their progress as events on
	// the owning object (--runner-events).
	RunnerEvents bool
	// DriftDefaultInterval applies when an object sets no drift interval.
	DriftDefaultInterval time.Duration
	// ClusterOperationGate keeps a TerraformCluster's apply or destroy and
	// its machines' applies and destroys apart (--cluster-operation-gate).
	ClusterOperationGate bool
	// StateBackups is how many state backups to keep per object
	// (--state-backups); 0 disables backups.
	StateBackups int

	// Diagnostics holds CAPI's diagnostics and TLS flags.
	Diagnostics flags.ManagerOptions
	// Logs holds the logsv1 flags.
	Logs *logsv1.LoggingConfiguration
	// FeatureGates holds --feature-gates.
	FeatureGates featuregate.MutableFeatureGate
}

// NewOptions returns options with the logging configuration and feature
// gates allocated, and the default log verbosity set to 2; Flags sets every
// other default.
func NewOptions() *Options {
	logs := logsv1.NewLoggingConfiguration()
	logs.Verbosity = 2
	return &Options{
		Logs:         logs,
		FeatureGates: feature.NewGates(),
	}
}

// Flags registers every manager flag with its default, grouped into named
// sections (generic, leader election, webhook, runner, diagnostics, logs,
// feature gates), and returns the resulting cliflag.NamedFlagSets for the
// caller to add to a command's flag set and to use for --help output.
func (o *Options) Flags() cliflag.NamedFlagSets {
	fss := cliflag.NamedFlagSets{}

	generic := fss.FlagSet("generic")
	generic.StringVar(&o.Namespace, "namespace", "",
		"Namespace that the controller watches to reconcile objects. If unspecified, the controller watches all namespaces.")
	generic.StringVar(&o.WatchFilter, "watch-filter", "",
		fmt.Sprintf("Label value that the controller watches to reconcile objects. Label key is always %s. If unspecified, the controller watches all objects.", clusterv1.WatchLabel))
	generic.DurationVar(&o.SyncPeriod, "sync-period", 10*time.Minute,
		"The minimum interval at which watched resources are reconciled (e.g. 15m)")
	generic.IntVar(&o.TerraformClusterConcurrency, "terraformcluster-concurrency", 10,
		"Number of TerraformClusters to process simultaneously")
	generic.IntVar(&o.TerraformMachineConcurrency, "terraformmachine-concurrency", 10,
		"Number of TerraformMachines to process simultaneously")
	generic.IntVar(&o.TerraformMachineTemplateConcurrency, "terraformmachinetemplate-concurrency", 10,
		"Number of TerraformMachineTemplates to process simultaneously")
	generic.IntVar(&o.TerraformMachinePoolConcurrency, "terraformmachinepool-concurrency", 10,
		"Number of TerraformMachinePools to process simultaneously")
	generic.StringVar(&o.HealthAddr, "health-addr", ":9440", "The address the health endpoint binds to.")
	generic.StringVar(&o.ProfilerAddress, "profiler-address", "",
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")
	gofs := goflag.NewFlagSet("kubeconfig", goflag.ContinueOnError)
	ctrl.RegisterFlags(gofs)
	generic.AddGoFlagSet(gofs)

	leaderElection := fss.FlagSet("leader election")
	leaderElection.BoolVar(&o.LeaderElect, "leader-elect", false,
		"Enable leader election for the controller manager, ensuring there is only one active manager.")
	leaderElection.DurationVar(&o.LeaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string)")
	leaderElection.DurationVar(&o.LeaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership before giving up (duration string)")
	leaderElection.DurationVar(&o.LeaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string)")

	webhookFS := fss.FlagSet("webhook")
	webhookFS.IntVar(&o.WebhookPort, "webhook-port", 9443, "Port the webhook server listens on.")
	webhookFS.StringVar(&o.WebhookCertDir, "webhook-cert-dir", DefaultWebhookCertDir, "Directory holding the webhook server's serving certificate and key (mounted from the cert-manager Secret).")
	webhookFS.StringVar(&o.WebhookCertName, "webhook-cert-name", "tls.crt", "File name of the serving certificate in --webhook-cert-dir.")
	webhookFS.StringVar(&o.WebhookKeyName, "webhook-key-name", "tls.key", "File name of the serving key in --webhook-cert-dir.")

	runner := fss.FlagSet("runner")
	runner.StringVar(&o.RunnerImage, "runner-image", "",
		fmt.Sprintf("Image of the init container that injects the runner binary into Jobs. Defaults to $%s, the manager's own image.", ManagerImageEnv))
	runner.BoolVar(&o.RunnerEvents, "runner-events", true,
		"Have Job runners emit progress events (RunStarted, Step*, PlanSummary, ResourcesChanged, RunFinished) on the owning Terraform* object. Needs events create in the runner ClusterRole.")
	runner.DurationVar(&o.DriftDefaultInterval, "drift-default-interval", 30*time.Minute,
		"Drift check interval for objects that set none.")
	runner.BoolVar(&o.ClusterOperationGate, "cluster-operation-gate", true,
		"Keep a TerraformCluster's apply or destroy and its machines' applies and destroys from running at once, through a per-Cluster write Lease. The per-object run Lease is always on.")
	runner.IntVar(&o.StateBackups, "state-backups", 5,
		"State backups to keep per object: every new state serial is copied into captf-state-backup-* Secrets and older copies are pruned. 0 takes no backups (existing ones stay and can still be restored).")

	flags.AddManagerOptions(fss.FlagSet("diagnostics"), &o.Diagnostics)
	logsv1.AddFlags(o.Logs, fss.FlagSet("logs"))
	o.FeatureGates.AddFlag(fss.FlagSet("feature gates"))

	return fss
}

// Complete fills values that come from the environment: --runner-image
// defaults to $CAPTF_MANAGER_IMAGE, looked up through lookupEnv (os.LookupEnv
// in production, a fake in tests), and ManagerUser comes from
// $POD_NAMESPACE and $SERVICE_ACCOUNT_NAME.
func (o *Options) Complete(lookupEnv func(string) (string, bool)) {
	ns, _ := lookupEnv(PodNamespaceEnv)
	sa, _ := lookupEnv(ServiceAccountEnv)
	if ns != "" && sa != "" {
		o.ManagerUser = "system:serviceaccount:" + ns + ":" + sa
	}
	if o.RunnerImage == "" {
		if v, ok := lookupEnv(ManagerImageEnv); ok {
			o.RunnerImage = v
		}
	}
}

// Validate rejects option values the manager cannot run with and returns the
// joined error describing every violation found, or nil when o is valid.
func (o *Options) Validate() error {
	var errs []error
	if o.RunnerImage == "" {
		errs = append(errs, fmt.Errorf("--runner-image is empty and $%s is not set: Jobs need the runner image", ManagerImageEnv))
	} else if _, err := reference.ParseNormalizedNamed(o.RunnerImage); err != nil {
		errs = append(errs, fmt.Errorf("--runner-image %q: %w", o.RunnerImage, err))
	}
	for name, v := range map[string]int{
		"terraformcluster-concurrency":         o.TerraformClusterConcurrency,
		"terraformmachine-concurrency":         o.TerraformMachineConcurrency,
		"terraformmachinetemplate-concurrency": o.TerraformMachineTemplateConcurrency,
		"terraformmachinepool-concurrency":     o.TerraformMachinePoolConcurrency,
	} {
		if v < 1 {
			errs = append(errs, fmt.Errorf("--%s must be at least 1, got %d", name, v))
		}
	}
	if o.StateBackups < 0 {
		errs = append(errs, fmt.Errorf("--state-backups must not be negative, got %d", o.StateBackups))
	}
	for name, v := range map[string]time.Duration{
		"sync-period":            o.SyncPeriod,
		"drift-default-interval": o.DriftDefaultInterval,
	} {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("--%s must be positive, got %s", name, v))
		}
	}
	if err := kerrors.NewAggregate(errs); err != nil {
		return fmt.Errorf("manager: invalid flags: %w", err)
	}
	return nil
}

// ManagerOptions builds the controller-runtime manager options: diagnostics
// with authn/authz, the webhook server, leader election, and the cache
// scoping CacheOptions applies (an optional namespace restriction plus
// label-selected Secret and Job informers), with scheme as the manager's
// typed Scheme. It returns those ctrl.Options, or an error when o's
// diagnostics flags (TLS version, cipher suites) are invalid.
func (o *Options) ManagerOptions(scheme *runtime.Scheme) (ctrl.Options, error) {
	tlsOpts, metricsOpts, err := flags.GetManagerOptions(o.Diagnostics)
	if err != nil {
		return ctrl.Options{}, fmt.Errorf("manager: diagnostics flags: %w", err)
	}

	cacheOpts := captfmanager.CacheOptions(o.Namespace)
	syncPeriod := o.SyncPeriod
	cacheOpts.SyncPeriod = &syncPeriod
	gracefulShutdown := GracefulShutdownTimeout
	lease, renew, retry := o.LeaderElectionLeaseDuration, o.LeaderElectionRenewDeadline, o.LeaderElectionRetryPeriod

	return ctrl.Options{
		Scheme:                     scheme,
		LeaderElection:             o.LeaderElect,
		LeaderElectionID:           LeaderElectionID,
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		// The leader gives up the Lease on shutdown, so the standby takes
		// over at once instead of after LeaseDuration. Safe because Run
		// returns, and the process exits, as soon as the manager stops.
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       &gracefulShutdown,
		LeaseDuration:              &lease,
		RenewDeadline:              &renew,
		RetryPeriod:                &retry,
		HealthProbeBindAddress:     o.HealthAddr,
		PprofBindAddress:           o.ProfilerAddress,
		Metrics:                    *metricsOpts,
		Cache:                      cacheOpts,
		Client:                     client.Options{Cache: &client.CacheOptions{DisableFor: captfmanager.UncachedObjects()}},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:     o.WebhookPort,
			CertDir:  o.WebhookCertDir,
			CertName: o.WebhookCertName,
			KeyName:  o.WebhookKeyName,
			TLSOpts:  append([]func(*tls.Config){}, tlsOpts...),
		}),
	}, nil
}
