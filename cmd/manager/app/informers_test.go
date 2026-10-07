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
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2/textlogger"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"

	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	captfmanager "github.com/captf-io/cluster-api-provider-terraform/internal/manager"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
)

// recordingCache is a ctrlcache.Cache that never contacts an API server:
// every informer it hands out is a synced, empty FakeInformer, and it
// records the type of each informer requested, by a watch or an index.
type recordingCache struct {
	*informertest.FakeInformers

	mu    sync.Mutex
	types map[string]bool
}

// newRecordingCache returns an empty recordingCache.
func newRecordingCache() *recordingCache {
	return &recordingCache{FakeInformers: &informertest.FakeInformers{}, types: map[string]bool{}}
}

// record notes the informer type of obj: "meta:<Kind>" for a
// metav1.PartialObjectMetadata, its Go type otherwise.
func (c *recordingCache) record(obj client.Object) {
	name := fmt.Sprintf("%T", obj)
	if m, ok := obj.(*metav1.PartialObjectMetadata); ok {
		name = "meta:" + m.GroupVersionKind().Kind
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.types[name] = true
}

// recorded returns the sorted informer types requested so far.
func (c *recordingCache) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.types))
	for k := range c.types {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// GetInformer records obj's type and returns a new synced FakeInformer.
func (c *recordingCache) GetInformer(_ context.Context, obj client.Object, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	c.record(obj)
	return controllertest.NewFakeInformer(controllertest.Synced), nil
}

// GetInformerForKind records gvk and returns a new synced FakeInformer.
func (c *recordingCache) GetInformerForKind(_ context.Context, gvk schema.GroupVersionKind, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.types["kind:"+gvk.Kind] = true
	return controllertest.NewFakeInformer(controllertest.Synced), nil
}

// IndexField records obj's type, since indexing a type starts its
// informer, and returns nil.
func (c *recordingCache) IndexField(_ context.Context, obj client.Object, _ string, _ client.IndexerFunc) error {
	c.record(obj)
	return nil
}

// Start blocks until ctx is done, as a real cache does, and returns nil.
func (c *recordingCache) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// WaitForCacheSync reports the cache synced.
func (c *recordingCache) WaitForCacheSync(context.Context) bool { return true }

// syncWriter is an io.Writer safe for concurrent use that keeps what is
// written to it.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p under the lock and returns len(p) and a nil error.
func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// count returns how many times s was written.
func (w *syncWriter) count(s string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(w.buf.String(), s)
}

// TestNoTypedSecretInformer starts every controller setup registers, with
// the field indexes, against recording caches, and proves no watch or
// index requests a typed Secret or ConfigMap informer from the manager's
// cache or the variables cache: the caches are keyed per GVK, so one
// would hold every state, backup, inputs or variables payload under the
// same selector (findings 6, 7). The managed Secrets and the variables
// sources must be watched as metadata.
func TestNoTypedSecretInformer(t *testing.T) {
	restoreMetricsRegistry(t)
	opts := testOpts(t)
	scheme, err := captfmanager.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	mgrOpts, err := opts.ManagerOptions(scheme)
	if err != nil {
		t.Fatal(err)
	}
	testConfigure(t)(&mgrOpts)
	main, vars := newRecordingCache(), newRecordingCache()
	mgrOpts.NewCache = func(*rest.Config, ctrlcache.Options) (ctrlcache.Cache, error) { return main, nil }
	mgrOpts.Metrics.BindAddress = "0"
	mgrOpts.LeaderElection = false
	logs := &syncWriter{}
	mgrOpts.Logger = textlogger.NewLogger(textlogger.NewConfig(textlogger.Output(logs)))

	mgr, err := ctrl.NewManager(unreachableConfig(), mgrOpts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := shared.SetupIndexes(ctx, mgr); err != nil {
		t.Fatal(err)
	}
	deps := newDeps(mgr, opts, metrics.New())
	deps.VariablesCache = vars
	if err := setupReconcilers(ctx, mgr, opts, deps); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()

	// A controller logs this once every watch of it has an informer and
	// has synced.
	const controllers = 5
	deadline := time.Now().Add(30 * time.Second)
	for logs.count(`"Starting workers"`) < controllers {
		select {
		case err := <-done:
			t.Fatalf("manager stopped before its controllers started: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d controllers started; main cache %v, variables cache %v",
				logs.count(`"Starting workers"`), controllers, main.recorded(), vars.recorded())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("manager: %v", err)
	}

	for name, c := range map[string]*recordingCache{"main": main, "variables": vars} {
		for _, typ := range c.recorded() {
			if typ == "*v1.Secret" || typ == "*v1.ConfigMap" || typ == "kind:Secret" || typ == "kind:ConfigMap" {
				t.Errorf("%s cache: a typed %s informer was requested; it would hold every payload", name, typ)
			}
		}
	}
	if got := main.recorded(); !slices.Contains(got, "meta:Secret") {
		t.Errorf("main cache informers = %v, want a metadata Secret informer", got)
	}
	if got, want := vars.recorded(), []string{"meta:ConfigMap", "meta:Secret"}; !slices.Equal(got, want) {
		t.Errorf("variables cache informers = %v, want %v", got, want)
	}
}
