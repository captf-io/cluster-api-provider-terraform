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

package app

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/captf-io/cluster-api-provider-terraform/cmd/manager/app/options"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/sweep"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/terraformcluster"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/terraformclusteridentity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/terraformmachine"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/terraformmachinepool"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/terraformmachinetemplate"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	captfmanager "github.com/captf-io/cluster-api-provider-terraform/internal/manager"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
	"github.com/captf-io/cluster-api-provider-terraform/internal/webhooks"
)

// managerName is the manager binary's command name, used as both the
// cobra command's Use and the leading word of its --version output.
const managerName = "manager"

// NewManagerCommand builds the manager's cobra command: it registers every
// named flag set options.Options.Flags returns, plus the --version flag
// from k8s.io/component-base/version/verflag and the standard --help flag,
// on a "global" section, and wires RunE to validate and run the manager.
// It returns the built *cobra.Command, unexecuted.
func NewManagerCommand() *cobra.Command {
	opts := options.NewOptions()

	cmd := &cobra.Command{
		Use:          managerName,
		Short:        "Run the CAPTF controller manager",
		SilenceUsage: true,
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if arg != "" {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch cmd.Flags().Lookup("version").Value.String() {
			case "true":
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", cmd.Root().Name(), version.Get())
				return nil
			case "raw":
				fmt.Fprintf(cmd.OutOrStdout(), "%#v\n", version.Get())
				return nil
			}

			if err := logsv1.ValidateAndApply(opts.Logs, opts.FeatureGates); err != nil {
				return fmt.Errorf("logging flags: %w", err)
			}
			ctrl.SetLogger(klog.Background())
			setupLogger := klog.LoggerWithName(klog.Background(), "setup")
			cliflag.PrintFlags(cmd.Flags())

			opts.Complete(os.LookupEnv)
			if err := opts.Validate(); err != nil {
				return err
			}

			info := version.Get()
			setupLogger.Info("Starting CAPTF manager", "version", info.GitVersion, "commit", info.GitCommit, "contract", contract.Version)

			ctx := klog.NewContext(ctrl.SetupSignalHandler(), setupLogger)
			return Run(ctx, opts)
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true

	namedFlagSets := opts.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())

	fs := cmd.Flags()
	fs.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	for _, name := range namedFlagSets.Order {
		fs.AddFlagSet(namedFlagSets.FlagSet(name))
	}

	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, namedFlagSets, cols)

	cmd.AddCommand(newVersionCommand())

	return cmd
}

// newVersionCommand builds the `manager version` subcommand, which prints
// the same line `manager --version` does. It returns the built
// *cobra.Command.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the manager's version and exit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", cmd.Root().Name(), version.Get())
			return nil
		},
	}
}

// Run builds the controller-runtime manager and every webhook, reconciler
// and background runnable from opts, and starts it. ctx carries the setup
// logger installed by NewManagerCommand's RunE and governs the manager's
// lifecycle; opts is the completed, validated manager configuration. Run
// blocks until ctx is canceled and returns nil on a clean shutdown, or the
// error from whichever setup or run step failed, wrapped with what that
// step was.
func Run(ctx context.Context, opts *options.Options) error {
	setupLogger := klog.FromContext(ctx)

	mgr, err := setup(ctx, ctrl.GetConfigOrDie, opts, nil)
	if err != nil {
		return err
	}

	setupLogger.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}

// warnManagerUserUnset logs a warning to log when managerUser is empty. The
// TerraformMachine webhook lets only that user set spec.providerID, so with
// it unset the webhook refuses the controller's own providerID writes and
// no machine finishes provisioning. It is a warning, not a startup failure,
// because every other part of the manager still works.
func warnManagerUserUnset(log klog.Logger, managerUser string) {
	if managerUser != "" {
		return
	}
	log.Info("WARNING: the manager's identity is unknown, so the TerraformMachine webhook will reject its own "+
		"spec.providerID writes and no machine will finish provisioning; set "+options.PodNamespaceEnv+" and "+
		options.ServiceAccountEnv+" on the manager Deployment from the downward API (metadata.namespace and "+
		"spec.serviceAccountName)",
		"podNamespaceEnv", options.PodNamespaceEnv, "serviceAccountEnv", options.ServiceAccountEnv)
}

// setup builds the controller-runtime manager and every webhook, reconciler
// and background runnable from opts, but never starts it: that is left to
// the caller, so tests can exercise setup against an unstarted manager. ctx
// builds the reconcilers' watches and is not otherwise retained. getConfig
// is called immediately before ctrl.NewManager, after opts and metrics
// setup, so a bad kubeconfig is reported only once they succeed; Run passes
// ctrl.GetConfigOrDie. configure, when non-nil, adjusts the ctrl.Options
// built from opts before ctrl.NewManager; Run passes nil, and tests use it
// to install a static RESTMapper and skip controller-runtime's global
// controller-name check. It returns the configured ctrl.Manager, or the
// error from whichever setup step failed, wrapped with what that step was.
func setup(ctx context.Context, getConfig func() *rest.Config, opts *options.Options,
	configure func(*ctrl.Options)) (ctrl.Manager, error) {
	scheme, err := captfmanager.NewScheme()
	if err != nil {
		return nil, err
	}
	mgrOpts, err := opts.ManagerOptions(scheme)
	if err != nil {
		return nil, err
	}
	if configure != nil {
		configure(&mgrOpts)
	}

	// captfReg holds CAPTF's own series; metrics.Install replaces
	// ctrlmetrics.Registry with a Bridge that still registers controller-
	// runtime's own instrumentation on the registry it found, but serves
	// captfReg and component-base's legacyregistry (kubernetes_feature_enabled
	// and any other component-base series) alongside it on the same
	// diagnostics endpoint. It must run before ctrl.NewManager, which wires
	// controller and client-go instrumentation onto ctrlmetrics.Registry.
	captfReg := cbmetrics.NewKubeRegistry()
	metrics.Install(captfReg)
	if err := metrics.Register(captfReg, version.Get()); err != nil {
		return nil, err
	}
	rec := metrics.New()
	if err := rec.Register(captfReg); err != nil {
		return nil, err
	}

	mgr, err := ctrl.NewManager(getConfig(), mgrOpts)
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}
	rec.ActiveJobs().Bind(mgr.GetCache())
	warnManagerUserUnset(klog.FromContext(ctx), opts.ManagerUser)
	// One schema cache: the controllers fill it as they inspect images, and
	// the webhooks read it without ever contacting a registry.
	schemas := imageinspect.NewSchemaCache()
	if err := webhooks.SetupWebhooks(mgr, opts.ManagerUser, schemas); err != nil {
		return nil, err
	}
	if err := shared.SetupIndexes(ctx, mgr); err != nil {
		return nil, err
	}
	deps := newDeps(mgr, opts, rec)
	deps.Schemas = schemas
	// The spec.variablesFrom watches need their own label-scoped cache.
	varCache, err := ctrlcache.New(mgr.GetConfig(),
		captfmanager.VariablesCacheOptions(opts.Namespace, scheme, mgr.GetRESTMapper(), mgr.GetHTTPClient()))
	if err != nil {
		return nil, fmt.Errorf("create variables cache: %w", err)
	}
	if err := mgr.Add(captfmanager.VariablesCache{Cache: varCache}); err != nil {
		return nil, fmt.Errorf("add variables cache: %w", err)
	}
	deps.VariablesCache = varCache
	if err := setupReconcilers(ctx, mgr, opts, deps); err != nil {
		return nil, err
	}
	if err := mgr.Add(sweep.New(mgr, opts.SyncPeriod, opts.Namespace)); err != nil {
		return nil, fmt.Errorf("add orphan sweep: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return nil, fmt.Errorf("add health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		return nil, fmt.Errorf("add ready check: %w", err)
	}

	return mgr, nil
}

// setupReconcilers registers every controller with mgr, using ctx to build
// their watches: the TerraformCluster, TerraformMachine,
// TerraformMachineTemplate, TerraformMachinePool and
// TerraformClusterIdentity reconcilers, with deps and the concurrency and
// watch-filter settings of opts. It is split from setup so a test can
// start the controllers against a fake cache and see which informers their
// watches request. It returns the first registration error.
func setupReconcilers(ctx context.Context, mgr ctrl.Manager, opts *options.Options, deps shared.Deps) error {
	if err := (&terraformcluster.Reconciler{Deps: deps}).SetupWithManager(ctx, mgr,
		controller.Options{MaxConcurrentReconciles: opts.TerraformClusterConcurrency}); err != nil {
		return err
	}
	if err := (&terraformmachine.Reconciler{Deps: deps}).SetupWithManager(ctx, mgr,
		controller.Options{MaxConcurrentReconciles: opts.TerraformMachineConcurrency}); err != nil {
		return err
	}
	if err := (&terraformmachinetemplate.Reconciler{Deps: deps}).SetupWithManager(mgr,
		controller.Options{MaxConcurrentReconciles: opts.TerraformMachineTemplateConcurrency}); err != nil {
		return err
	}
	if err := (&terraformmachinepool.Reconciler{Deps: deps}).SetupWithManager(ctx, mgr,
		controller.Options{MaxConcurrentReconciles: opts.TerraformMachinePoolConcurrency}); err != nil {
		return err
	}
	// Status only (Ready, status.namespaces); the source Secret is re-read
	// every terraformclusteridentity.DefaultRequeueAfter, or
	// NotReadyRequeueAfter while it is missing or incomplete.
	return (&terraformclusteridentity.Reconciler{
		Client:      mgr.GetClient(),
		Cache:       mgr.GetCache(),
		APIReader:   mgr.GetAPIReader(),
		WatchFilter: opts.WatchFilter,
		Recorder:    deps.Recorder,
	}).SetupWithManager(mgr, controller.Options{})
}

// newDeps builds the reconcilers' shared dependencies from the started
// manager mgr, the validated options opts and
// the metrics recorder rec, and returns the resulting shared.Deps: Secrets
// are read live through mgr's default client, and Job pods through its API
// reader, while RunnerImage, RunnerEvents, DriftDefault, WatchFilter,
// ClusterOperationGate, StateBackups, MaxActiveJobs and ClusterMaxActiveJobs are copied from opts and Metrics is
// rec.
func newDeps(mgr ctrl.Manager, opts *options.Options, rec *metrics.Recorder) shared.Deps {
	c, apiReader := mgr.GetClient(), mgr.GetAPIReader()
	return shared.Deps{
		Client:       c,
		APIReader:    apiReader,
		Scheme:       mgr.GetScheme(),
		Recorder:     mgr.GetEventRecorder("captf-manager"),
		Metrics:      rec,
		Jobs:         jobs.NewRunner(c, apiReader),
		State:        state.NewReader(c),
		Clock:        clock.RealClock{},
		Inspector:    imageinspect.FallbackInspector{Inner: imageinspect.Remote{}},
		RunnerImage:  opts.RunnerImage,
		RunnerEvents: opts.RunnerEvents,
		DriftDefault: opts.DriftDefaultInterval,
		WatchFilter:  opts.WatchFilter,

		ClusterOperationGate: opts.ClusterOperationGate,
		StateBackups:         opts.StateBackups,
		MaxActiveJobs:        opts.MaxActiveJobs,
		ClusterMaxActiveJobs: opts.ClusterMaxActiveJobs,
	}
}
