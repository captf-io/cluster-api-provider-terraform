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

// Package kubewait polls a cluster for the states an end-to-end test waits
// on: a condition on any object, a field value, an object's disappearance,
// an Event, and a finished Job.
//
// Every poller checks at once, then every Options.Interval, prints a
// progress line to Options.Out every Options.ReportEvery, honors the
// caller's context, and gives up after Options.Timeout. The timeout error
// names what was awaited and carries the last observation, so a failed wait
// says what the object looked like rather than only that time ran out.
// Objects are unstructured and read through the dynamic client, so the test
// module never imports CAPI or CAPTF Go types. Transient read errors are
// reported as the observation and retried, not returned. Every error is
// prefixed "kubewait:" and wraps its cause.
package kubewait
