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

package health

import (
	"context"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// now is the clock of the lease checks; tests replace it.
var now = time.Now

// LeaseHeld checks that the Lease name in namespace is held by an
// identity starting with holderPrefix (normally the manager's pod name)
// and that its renewTime is no older than maxAge. It reads the lease with
// c under ctx. It returns nil when both
// hold, else an error describing each failure.
func LeaseHeld(ctx context.Context, c wait.Clients, namespace, name, holderPrefix string, maxAge time.Duration) error {
	l, err := c.Kube.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("health: get lease %s/%s: %w", namespace, name, err)
	}
	if problems := evalLeaseHeld(l, holderPrefix, maxAge, now()); len(problems) > 0 {
		return fmt.Errorf("health: lease %s/%s not held:\n  %s", namespace, name, strings.Join(problems, "\n  "))
	}
	return nil
}

// evalLeaseHeld is the pure LeaseHeld check of l against holderPrefix and
// maxAge at time at. It returns one message per problem, nil when held.
func evalLeaseHeld(l *coordinationv1.Lease, holderPrefix string, maxAge time.Duration, at time.Time) []string {
	var out []string
	holder := ""
	if l.Spec.HolderIdentity != nil {
		holder = *l.Spec.HolderIdentity
	}
	if holder == "" || !strings.HasPrefix(holder, holderPrefix) {
		out = append(out, fmt.Sprintf("holderIdentity %q does not start with %q", holder, holderPrefix))
	}
	if l.Spec.RenewTime == nil {
		out = append(out, "no renewTime")
	} else if age := at.Sub(l.Spec.RenewTime.Time); age > maxAge {
		out = append(out, fmt.Sprintf("renewTime %s is %s old, max %s", l.Spec.RenewTime.UTC().Format(time.RFC3339Nano), age.Round(time.Millisecond), maxAge))
	}
	return out
}

// LeaseRenewing checks that the renewTime of the Lease name in namespace
// advances within the duration within, using the clients c. It reads the lease, then polls
// until the renewTime moves forward, and returns nil when it does, else an
// error when within passes first or ctx ends.
func LeaseRenewing(ctx context.Context, c wait.Clients, namespace, name string, within time.Duration) error {
	get := func() (time.Time, error) {
		l, err := c.Kube.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return time.Time{}, fmt.Errorf("health: get lease %s/%s: %w", namespace, name, err)
		}
		if l.Spec.RenewTime == nil {
			return time.Time{}, nil
		}
		return l.Spec.RenewTime.Time, nil
	}
	first, err := get()
	if err != nil {
		return err
	}
	poll := within / 20
	if poll < 10*time.Millisecond {
		poll = 10 * time.Millisecond
	}
	if poll > time.Second {
		poll = time.Second
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("health: lease %s/%s: %w", namespace, name, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("health: lease %s/%s not renewing: renewTime %s did not advance within %s", namespace, name, first.UTC().Format(time.RFC3339Nano), within)
		case <-tick.C:
			cur, err := get()
			if err != nil {
				return err
			}
			if cur.After(first) {
				return nil
			}
		}
	}
}
