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

// Package state reads Terraform/OpenTofu state written by the kubernetes
// backend straight from its Secrets, and owns everything CAPTF derives from
// state identity: the backend secret_suffix, the state and lock Secret/Lease
// names, and the labels the backend and CAPTF itself apply.
//
// Reader (NewReader, State, Output) lists an object's state Secrets,
// reassembles them in chunk order and parses the Terraform state v4 file,
// reporting ErrNoState, ErrStateInconsistent, ErrStateEncrypted,
// ErrUnsupportedStateVersion or ErrStateCorrupt rather than partial or wrong
// data. Adopt re-labels state Secrets with an ownerReference after every
// successful apply, since Terraform's own chunk writes carry only the
// backend's labels; State.Metadata lets the reconciler re-own a chunk
// between applies without listing the chunks again. Cleanup removes every state Secret and the lock Lease
// after a successful destroy, since neither backend does that itself.
// TakeBackup, ListBackups, FindBackup and PruneBackups copy state Secrets
// verbatim into captf-state-backup-* Secrets and manage their retention.
//
// Labels come only from immutable identity (namespace, kind and name), never
// a mutable field or the UID, so state survives a clusterctl move and a
// changed identity is treated as a different, unrelated state. Outputs may
// be sensitive: state.Output and backup values are never logged.
package state
