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

package conditions

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// MaxMessage is the API server's limit on a condition message, in bytes
// (metav1.Condition's maxLength).
const MaxMessage = 32768

// Conditioned is an object whose conditions ClampMessages bounds.
type Conditioned interface {
	// GetConditions returns the object's conditions.
	GetConditions() []metav1.Condition
	// SetConditions replaces the object's conditions.
	SetConditions(conditions []metav1.Condition)
}

// ClampMessages cuts every condition message of obj longer than
// MaxMessage to fit (strutil.Truncate). One message over the limit makes
// the API server reject the whole status write, which then freezes every
// condition of the object and fails every reconcile: messages quote module
// output, registry errors and image labels, which nothing else bounds as
// tightly. It reports whether it cut any.
func ClampMessages(obj Conditioned) bool {
	conds := obj.GetConditions()
	cut := false
	for i := range conds {
		if len(conds[i].Message) > MaxMessage {
			conds[i].Message = strutil.Truncate(conds[i].Message, MaxMessage)
			cut = true
		}
	}
	if cut {
		obj.SetConditions(conds)
	}
	return cut
}
