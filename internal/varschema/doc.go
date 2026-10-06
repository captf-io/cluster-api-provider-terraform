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

// Package varschema is the JSON Schema subset an image publishes in its
// io.captf.variables-schema label to describe the module's user variables,
// and the small validator the manager runs over merged variables before
// any Job starts.
//
// The subset is type, properties, required, items, prefixItems and
// additionalProperties; nothing else is emitted or understood. Null is
// accepted everywhere, because the schema does not record nullable.
//
// The package imports only the standard library, because tfcapi-lint
// shares it.
package varschema
