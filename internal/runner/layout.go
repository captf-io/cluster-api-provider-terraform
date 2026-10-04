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

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrImageLayout marks an image that does not follow the image contract; it
// is reported as error.kind "image-layout".
var ErrImageLayout = errors.New("image layout")

// moduleSuffixes are the file extensions Preflight looks for at the module
// directory's top level.
var moduleSuffixes = []string{".tf", ".tf.json", ".tofu", ".tofu.json"}

// Preflight checks the image before any step: moduleDir holds at least one
// Terraform/OpenTofu file at its top level, and bin[0] resolves to an
// executable. The provider mirror is optional and not checked. It returns a
// non-nil, ErrImageLayout-wrapped error when either check fails.
func Preflight(moduleDir string, bin []string) error {
	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		return fmt.Errorf("%w: module directory %s: %w", ErrImageLayout, moduleDir, err)
	}
	found := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, suffix := range moduleSuffixes {
			if strings.HasSuffix(e.Name(), suffix) {
				found = true
			}
		}
	}
	if !found {
		return fmt.Errorf("%w: %s has no %v file", ErrImageLayout, moduleDir, moduleSuffixes)
	}
	if len(bin) == 0 || bin[0] == "" {
		return fmt.Errorf("%w: empty command", ErrImageLayout)
	}
	if strings.Contains(bin[0], "/") {
		info, err := os.Stat(bin[0])
		if err != nil {
			return fmt.Errorf("%w: command %s: %w", ErrImageLayout, bin[0], err)
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%w: command %s is not an executable file", ErrImageLayout, bin[0])
		}
		return nil
	}
	if _, err := exec.LookPath(bin[0]); err != nil {
		return fmt.Errorf("%w: command %s: %w", ErrImageLayout, bin[0], err)
	}
	return nil
}
