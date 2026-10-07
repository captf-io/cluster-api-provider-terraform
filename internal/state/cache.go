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

package state

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Bounds of the CachingReader.
const (
	// MaxCachedStates is how many parsed states a CachingReader keeps.
	MaxCachedStates = 512
	// MaxCachedBytes bounds the outputs a CachingReader keeps, summed over
	// its states.
	MaxCachedBytes = 64 << 20
	// MaxConcurrentReads is how many state payloads a CachingReader
	// fetches and decodes at once: each may take a few times MaxStateBytes
	// while it is decoded.
	MaxConcurrentReads = 4
)

// CachingReader is a Reader that reads an object's state Secrets live,
// but as metadata first: their names, UIDs and resourceVersions say
// whether the state changed since it last parsed it. Only then does it
// fetch the payloads and decode them, at most MaxConcurrentReads at a
// time; otherwise it returns the state it parsed. A pass over an idle
// object, and each of a cluster's machines reading the cluster's state,
// then costs a metadata list instead of the whole state. The metadata is
// listed live, never from an informer, so a state written just now (an
// apply's, or Adopt's own patch) is never answered from the cache.
type CachingReader struct {
	c     client.Reader
	inner Reader
	sem   chan struct{}

	mu      sync.Mutex
	entries map[string]*cachedState
	order   []string
	bytes   int
}

// cachedState is one parsed state of a CachingReader.
type cachedState struct {
	// fingerprint is what its Secrets' metadata was (fingerprintOf).
	fingerprint string
	st          *State
	// cost is what it counts against MaxCachedBytes.
	cost int
}

// NewCachingReader returns a CachingReader listing and reading Secrets
// live through c: a client whose Secret reads bypass any informer.
func NewCachingReader(c client.Reader) *CachingReader {
	return &CachingReader{c: c, inner: NewReader(c), sem: make(chan struct{}, MaxConcurrentReads), entries: map[string]*cachedState{}}
}

// Read returns the state of suffix in namespace, using ctx for the calls:
// the parsed state kept from an earlier read when the state Secrets'
// metadata is unchanged since, else what the underlying reader returns,
// kept for the next read when it parsed. It returns ErrNoState when no
// state Secret exists, and the underlying reader's errors.
func (r *CachingReader) Read(ctx context.Context, namespace, suffix string) (*State, error) {
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: Selector(suffix)}); err != nil {
		return nil, fmt.Errorf("state: list Secrets: %w", err)
	}
	key := namespace + "/" + suffix
	if len(list.Items) == 0 {
		r.forget(key)
		return nil, ErrNoState
	}
	metas := make([]metav1.ObjectMeta, len(list.Items))
	for i := range list.Items {
		metas[i] = list.Items[i].ObjectMeta
	}
	fp := fingerprintOf(metas)
	if st := r.lookup(key, fp); st != nil {
		return st, nil
	}

	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("state: wait to read: %w", ctx.Err())
	}
	st, err := r.inner.Read(ctx, namespace, suffix)
	<-r.sem
	if err != nil {
		r.forget(key)
		return nil, err
	}
	// Kept only as the metadata listed: a write between the two reads is
	// read again next time.
	if fingerprintOf(st.Metadata) == fp {
		r.store(key, fp, st)
	}
	return st, nil
}

// fingerprintOf returns what identifies the contents of the Secrets whose
// metadata is metas: each one's name, UID and resourceVersion, sorted by
// name. Any write to a Secret changes it.
func fingerprintOf(metas []metav1.ObjectMeta) string {
	parts := make([]string, 0, len(metas))
	for _, m := range metas {
		parts = append(parts, m.Name+"/"+string(m.UID)+"/"+m.ResourceVersion)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// lookup returns a copy of the state kept for key when it was parsed
// from Secrets of fingerprint fp, else nil.
func (r *CachingReader) lookup(key, fp string) *State {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok || e.fingerprint != fp {
		return nil
	}
	return e.st.clone()
}

// store keeps a copy of st for key, parsed from Secrets of fingerprint fp,
// evicting the oldest states beyond MaxCachedStates and MaxCachedBytes. A
// state larger than MaxCachedBytes on its own is not kept.
func (r *CachingReader) store(key, fp string, st *State) {
	cost := st.cost()
	if cost > MaxCachedBytes {
		r.forget(key)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked(key)
	r.entries[key] = &cachedState{fingerprint: fp, st: st.clone(), cost: cost}
	r.order = append(r.order, key)
	r.bytes += cost
	for len(r.order) > MaxCachedStates || r.bytes > MaxCachedBytes {
		r.dropLocked(r.order[0])
	}
}

// forget drops the state kept for key, if any.
func (r *CachingReader) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked(key)
}

// dropLocked drops the state kept for key, if any; r.mu is held.
func (r *CachingReader) dropLocked(key string) {
	e, ok := r.entries[key]
	if !ok {
		return
	}
	delete(r.entries, key)
	r.bytes -= e.cost
	r.order = slices.DeleteFunc(r.order, func(k string) bool { return k == key })
}

// Len returns how many states r keeps.
func (r *CachingReader) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// cost returns what st counts against MaxCachedBytes: its outputs' bytes.
func (st *State) cost() int {
	n := 0
	for k, o := range st.Outputs {
		n += len(k) + len(o.Value) + len(o.Type)
	}
	return n
}

// clone returns a copy of st that shares no slice or map with it; the
// output values, which no caller writes, are shared.
func (st *State) clone() *State {
	c := *st
	c.Outputs = maps.Clone(st.Outputs)
	c.Secrets = slices.Clone(st.Secrets)
	c.Metadata = make([]metav1.ObjectMeta, len(st.Metadata))
	for i := range st.Metadata {
		st.Metadata[i].DeepCopyInto(&c.Metadata[i])
	}
	return &c
}
