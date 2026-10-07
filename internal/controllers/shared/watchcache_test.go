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

package shared

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/internal/manager"
)

// secretMeta returns the metadata-only Secret a managed-Secret or variables
// watch delivers: om under the v1 Secret GVK (manager.SecretMeta).
func secretMeta(om metav1.ObjectMeta) *metav1.PartialObjectMetadata {
	m := manager.SecretMeta()
	m.ObjectMeta = om
	return m
}

// configMapMeta returns the metadata-only ConfigMap a variables watch
// delivers: om under the v1 ConfigMap GVK (manager.ConfigMapMeta).
func configMapMeta(om metav1.ObjectMeta) *metav1.PartialObjectMetadata {
	m := manager.ConfigMapMeta()
	m.ObjectMeta = om
	return m
}
