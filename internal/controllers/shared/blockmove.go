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

package shared

import (
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetBlockMove sets clusterctl's block-move annotation on obj and reports
// whether it changed. It is set before a Job is created and while one is
// active.
func SetBlockMove(obj client.Object) bool {
	a := obj.GetAnnotations()
	if _, ok := a[clusterctlv1.BlockMoveAnnotation]; ok {
		return false
	}
	if a == nil {
		a = map[string]string{}
	}
	a[clusterctlv1.BlockMoveAnnotation] = "true"
	obj.SetAnnotations(a)
	return true
}

// HasBlockMove reports whether obj carries the block-move annotation.
func HasBlockMove(obj client.Object) bool {
	_, ok := obj.GetAnnotations()[clusterctlv1.BlockMoveAnnotation]
	return ok
}

// ClearBlockMove removes the annotation from obj and reports whether it
// changed. It is cleared once no Job is active, also while paused:
// clusterctl move pauses first and then waits for it.
func ClearBlockMove(obj client.Object) bool {
	a := obj.GetAnnotations()
	if _, ok := a[clusterctlv1.BlockMoveAnnotation]; !ok {
		return false
	}
	delete(a, clusterctlv1.BlockMoveAnnotation)
	obj.SetAnnotations(a)
	return true
}
