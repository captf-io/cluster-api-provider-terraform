//go:build e2e

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

package noop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kubewait"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/tfstate"
)

// Stage 6's waits.
const (
	// destroyWait bounds a delete, from the request to the object gone.
	destroyWait = 6 * time.Minute
	// blockedWait bounds the TerraformCluster reporting DeletionBlocked.
	blockedWait = 2 * time.Minute
	// sweepWait bounds the namespace sweep: the last object's finalizer
	// removal sweeps the runner RBAC and Leases.
	sweepWait = 3 * time.Minute
)

// teardown is stage 6. Machines: deleting Machines A and B runs their
// destroy Jobs and removes the TerraformMachines with their state and
// inputs. Blocked cluster: the main TerraformCluster, deleted while the
// pool still carries its cluster-name label, reports
// DeletionBlocked=True (DependentsExist) and runs no destroy. Pool:
// deleting the MachinePool destroys it. Cluster: the blocked destroy
// then runs and the TerraformCluster goes, with both Clusters. Sweep: no
// state, inputs, plan-key or run Secret, no CAPTF Lease, no mirror and
// no runner RBAC remain. Identity: with no users left it can be deleted.
//
// The TerraformCluster is deleted directly, not through its Cluster:
// Cluster API deletes a Cluster's MachinePools and Machines before its
// infrastructure, so deleting the Cluster never reaches a blocked
// TerraformCluster.
// It runs under ctx and fails t on any problem.
func (s *suite) teardown(ctx context.Context, t *testing.T) {
	start := time.Now()
	for _, m := range []string{machineA, machineB} {
		s.deleteObject(ctx, t, objects.MachineGVR, m)
	}
	for _, m := range []struct {
		name string
		img  image
	}{{machineA, s.machineAImg}, {machineB, s.machineBImg}} {
		s.checkDestroyed(ctx, t, objects.KindTerraformMachine, m.name, m.img, start)
		s.waitGone(ctx, t, objects.MachineGVR, m.name, s.kubectlNS("get machine "+m.name+" -o yaml"))
	}

	s.deleteObject(ctx, t, objects.TerraformClusterGVR, clusterName)
	hint := s.hint(objects.KindTerraformCluster, clusterName)
	s.waitConditions(ctx, t, objects.TerraformClusterGVR, clusterName, []cond{{"DeletionBlocked", "True", "DependentsExist"}}, blockedWait, hint)
	if n := s.jobCount(ctx, t, objects.KindTerraformCluster, clusterName, "destroy"); n != 0 {
		t.Fatalf("TerraformCluster %s: expected no destroy Job while the pool exists (DeletionBlocked), observed %d; inspect: %s", clusterName, n, hint)
	}

	poolDeleted := time.Now()
	s.deleteObject(ctx, t, objects.MachinePoolGVR, poolName)
	s.checkDestroyed(ctx, t, objects.KindTerraformMachinePool, poolName, s.poolImg, poolDeleted)
	s.waitGone(ctx, t, objects.MachinePoolGVR, poolName, s.kubectlNS("get machinepool "+poolName+" -o yaml"))
	s.checkDestroyed(ctx, t, objects.KindTerraformCluster, clusterName, s.clusterImg, poolDeleted)
	s.deleteObject(ctx, t, objects.ClusterGVR, clusterName)
	s.waitGone(ctx, t, objects.ClusterGVR, clusterName, s.kubectlNS("get cluster "+clusterName+" -o yaml"))

	failDeleted := time.Now()
	s.deleteObject(ctx, t, objects.ClusterGVR, failName)
	s.checkDestroyed(ctx, t, objects.KindTerraformCluster, failName, s.clusterImg, failDeleted)
	s.waitGone(ctx, t, objects.ClusterGVR, failName, s.kubectlNS("get cluster "+failName+" -o yaml"))

	s.checkSweep(ctx, t)
	if err := s.c.Dynamic.Resource(objects.TerraformClusterIdentityGVR).Delete(ctx, s.identity, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("expected the webhook to allow deleting TerraformClusterIdentity %s once nothing uses it, observed: %v; inspect: %s", s.identity, err, s.kubectl("get terraformclusteridentity "+s.identity+" -o yaml"))
	}
	if err := kubewait.Gone(ctx, s.c.Dynamic, objects.TerraformClusterIdentityGVR, "", s.identity, waitOpts(t, time.Minute)); err != nil {
		t.Fatalf("%v; inspect: %s", err, s.kubectl("get terraformclusteridentity "+s.identity+" -o yaml"))
	}
}

// checkDestroyed waits for the CAPTF object kind name, whose deletion was
// requested at or after since, to go, then checks that a destroy Job ran
// on img's pinned reference (repo@digest) and succeeded, and that the
// object's state and inputs Secrets went with it. The destroy Job
// and its pod are deleted with the object seconds after they finish, so
// the evidence is the pod tracker's record and the JobSucceeded Event.
// It waits under ctx and reports through t.
func (s *suite) checkDestroyed(ctx context.Context, t *testing.T, kind, name string, img image, since time.Time) {
	t.Helper()
	hint := s.hint(kind, name)
	s.waitGone(ctx, t, gvrOf[kind], name, hint)
	var pods []podRecord
	for _, r := range s.tracker.records() {
		if r.kind == kind && r.owner == name && r.op == "destroy" && !r.created.Before(since.Truncate(time.Second)) {
			pods = append(pods, r)
		}
	}
	if len(pods) == 0 {
		t.Errorf("%s %s: expected a destroy Job pod after the delete at %s, observed none in the pod tracker; inspect: %s",
			kind, name, since.Format("15:04:05"), s.kubectlNS("get events --field-selector involvedObject.name="+name))
	}
	note := s.eventNote(ctx, t, name, "JobSucceeded", "destroy Job ")
	for _, p := range pods {
		// The pod can go with its object before the tracker sees it
		// Succeeded; the JobSucceeded Event naming its Job is the proof.
		if p.image != img.Pinned() || p.phase == corev1.PodFailed || !strings.HasPrefix(note, "destroy Job "+p.job+" ") {
			t.Errorf("%s %s: expected destroy pod %s (Job %s) to run %s (the pinned digest) and its Job to succeed, observed image %s, last phase %s %s, JobSucceeded Event %q",
				kind, name, p.name, p.job, img.Pinned(), p.image, p.phase, p.detail, note)
		}
	}
	job := note
	suffix := tfstate.SuffixFor(s.ns, kind, name)
	left, err := s.secretsLeft(ctx, "", tfstate.SecretName(suffix), "captf-inputs-"+kindShort[kind]+"-"+name, "captf-applied-"+kindShort[kind]+"-"+name)
	switch {
	case err != nil:
		t.Errorf("list Secrets of %s %s: %v", kind, name, err)
	case len(left) > 0:
		t.Errorf("%s %s: expected its state and inputs Secrets deleted with it, observed %v; inspect: %s", kind, name, left, s.kubectlNS("get secrets"))
	}
	t.Logf("%s %s gone; %s", kind, name, job)
}

// eventNote returns the message of an Event about the object name in the
// run's namespace with reason whose message starts with prefix, waiting
// up to eventWait. It fails t, and returns "", when none appears.
// It lists the Events under ctx.
func (s *suite) eventNote(ctx context.Context, t *testing.T, name, reason, prefix string) string {
	t.Helper()
	var found string
	err := eventually(ctx, t, fmt.Sprintf("event %s %q about %s", reason, prefix, name), eventWait, func(ctx context.Context) error {
		var seen []string
		if list, err := s.c.Kube.EventsV1().Events(s.ns).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range list.Items {
				e := &list.Items[i]
				if e.Regarding.Name == name && e.Reason == reason {
					if strings.HasPrefix(e.Note, prefix) {
						found = e.Note
						return nil
					}
					seen = append(seen, e.Note)
				}
			}
		}
		if list, err := s.c.Kube.CoreV1().Events(s.ns).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range list.Items {
				e := &list.Items[i]
				if e.InvolvedObject.Name == name && e.Reason == reason {
					if strings.HasPrefix(e.Message, prefix) {
						found = e.Message
						return nil
					}
					seen = append(seen, e.Message)
				}
			}
		}
		return fmt.Errorf("no %s Event starting %q about %s (messages seen for that reason: %q)", reason, prefix, name, seen)
	})
	if err != nil {
		t.Errorf("%v; inspect: %s", err, s.kubectlNS("get events --field-selector involvedObject.name="+name))
	}
	return found
}

// waitGone waits up to destroyWait for the object name of gvr in the
// run's namespace to go, failing t with hint otherwise.
// It waits under ctx.
func (s *suite) waitGone(ctx context.Context, t *testing.T, gvr schema.GroupVersionResource, name, hint string) {
	t.Helper()
	if err := kubewait.Gone(ctx, s.c.Dynamic, gvr, s.ns, name, waitOpts(t, destroyWait)); err != nil {
		t.Fatalf("%v; inspect: %s", err, hint)
	}
}

// jobCount returns the number of op Jobs of the CAPTF object kind name,
// failing t when they cannot be listed.
// It lists under ctx.
func (s *suite) jobCount(ctx context.Context, t *testing.T, kind, name, op string) int {
	t.Helper()
	list, err := s.c.Kube.BatchV1().Jobs(s.ns).List(ctx, metav1.ListOptions{LabelSelector: kubewait.JobsFor(kind, name, op).String()})
	if err != nil {
		t.Fatalf("list %s Jobs of %s %s: %v", op, kind, name, err)
	}
	return len(list.Items)
}

// checkSweep waits until the namespace holds no CAPTF Secret (state, the
// attempt and applied inputs records captf-inputs-* and captf-applied-*,
// per-run captf-run-*, plan key, backups, mirror), no Job, no CAPTF Lease,
// and no runner ServiceAccount or RoleBinding, failing t with what is left.
// It waits under ctx.
func (s *suite) checkSweep(ctx context.Context, t *testing.T) {
	t.Helper()
	err := eventually(ctx, t, "namespace "+s.ns+" swept", sweepWait, func(ctx context.Context) error {
		var left []string
		state, err := s.secretsLeft(ctx, stateLabel+"=true")
		if err != nil {
			return err
		}
		left = append(left, prefixAll("state Secret ", state)...)
		named, err := s.secretsLeft(ctx, "", "captf-")
		if err != nil {
			return err
		}
		left = append(left, prefixAll("Secret ", named)...)
		jobs, err := s.c.Kube.BatchV1().Jobs(s.ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for i := range jobs.Items {
			left = append(left, "Job "+jobs.Items[i].Name)
		}
		leases, err := s.c.Kube.CoordinationV1().Leases(s.ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for i := range leases.Items {
			left = append(left, "Lease "+leases.Items[i].Name)
		}
		if _, err := s.c.Kube.CoreV1().ServiceAccounts(s.ns).Get(ctx, runnerName, metav1.GetOptions{}); !isNotFound(err) {
			left = append(left, fmt.Sprintf("ServiceAccount %s (err %v)", runnerName, err))
		}
		if _, err := s.c.Kube.RbacV1().RoleBindings(s.ns).Get(ctx, runnerName, metav1.GetOptions{}); !isNotFound(err) {
			left = append(left, fmt.Sprintf("RoleBinding %s (err %v)", runnerName, err))
		}
		if len(left) > 0 {
			return errors.New(strings.Join(left, ", "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected no CAPTF Secret, Job, Lease or runner RBAC left once every Terraform* object is gone: %v; inspect: %s",
			err, s.kubectlNS("get secrets,jobs,leases,serviceaccounts,rolebindings"))
	}
}

// prefixAll returns each of names prefixed with p.
func prefixAll(p string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, p+n)
	}
	return out
}
