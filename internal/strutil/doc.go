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

// Package strutil holds string helpers shared by the manager and the
// runner, so a fix to one copy is a fix to every caller.
//
// Truncate cuts a string to a byte limit on a rune boundary, so a
// multi-byte character is never split. Event notes, run summaries, plan
// previews and the runner's failure summaries all go through it: each has
// a byte budget (an Event note, a status field the CRD caps) that a cut in
// the middle of a UTF-8 sequence would turn into invalid text.
//
// BoundedName builds a prefixed object name, such as an inputs or plan-key
// Secret name, kept within the 253-character DNS-1123 subdomain limit: a
// longer name keeps its start and gains a 16-digit SHA-256 suffix of the
// object name, so it stays deterministic and unique.
package strutil
