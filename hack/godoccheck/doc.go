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

// Command godoccheck enforces CAPTF's documentation rule on every Go file
// in the repository, test files included, and exits non-zero on the first
// run that finds a gap. `make verify-godoc` runs it, and `make verify`
// includes that target, so the rule holds for every change.
//
// The rule, per declaration:
//
//   - Every function, method, type (struct, interface or other), interface
//     method and top-level const or var group has a doc comment. A
//     function, method or type's comment starts with its name.
//   - Every named parameter of a function, method or interface method is
//     mentioned by name in its doc comment. Receivers and parameters named
//     "_" are exempt, and so is the conventional *testing.T, *testing.B or
//     *testing.F parameter of a Test, Benchmark or Fuzz function.
//   - A function or method that returns anything says what it returns: its
//     doc comment uses "return", "returns", "returned" or "reports".
//
// And per package: every package (except an external _test package) has a
// doc.go file holding its package comment, and that comment is a real
// overview of at least MinPackageDoc characters.
//
// Generated files (a "Code generated ... DO NOT EDIT." header, or a
// zz_generated. name) and the pinned tools under hack/tools are skipped.
//
// Usage:
//
//	godoccheck [dir]
//
// dir defaults to the current directory. Each finding is printed as
// file:line: message, followed by a count.
package main
