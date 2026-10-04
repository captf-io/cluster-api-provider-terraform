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
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Restore limits, the controller's state reader's (internal/state
// MaxChunks, MaxStateBytes), which the runner cannot import.
const (
	maxRestoreChunks = 32
	maxRestoreBytes  = 64 << 20
)

// restoreChunkDir is internal/jobs.RestoreChunkDir.
const restoreChunkDir = "restore"

// errRestoreInput is a backup the runner cannot push.
var errRestoreInput = errors.New("restore: invalid backup")

// AssembleRestore concatenates the n gzip chunks <configDir>/restore/0…n-1
// of a state backup (the kubernetes backend's Secret payloads, verbatim),
// decompresses them and writes the state JSON to <workDir>/RestoreStateFile,
// which the state push step reads. Like the controller's reader it stops at
// the end of the first gzip stream and refuses more than 64 MiB. It returns
// a non-nil, errRestoreInput-wrapped error when n is out of range or a
// chunk cannot be read, decompressed or parsed as JSON.
func AssembleRestore(configDir, workDir string, n int) error {
	return assembleRestore(configDir, workDir, n, maxRestoreBytes)
}

// assembleRestore does AssembleRestore's work on the chunks under configDir,
// writing to workDir, for n chunks, with the decompressed-size cap maxBytes
// a parameter so tests can exercise it without compressing 64 MiB. It
// returns the same errors AssembleRestore does, the cap error naming maxBytes.
func assembleRestore(configDir, workDir string, n, maxBytes int) error {
	if n < 1 || n > maxRestoreChunks {
		return fmt.Errorf("%w: %d chunks", errRestoreInput, n)
	}
	var payload []byte
	for i := range n {
		data, err := os.ReadFile(filepath.Join(configDir, restoreChunkDir, strconv.Itoa(i))) // #nosec G304 -- the runner's mounted config dir plus a loop index
		if err != nil {
			return fmt.Errorf("%w: chunk %d: %w", errRestoreInput, i, err)
		}
		payload = append(payload, data...)
	}
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: gzip: %w", errRestoreInput, err)
	}
	zr.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(zr, int64(maxBytes)+1))
	if err != nil {
		return fmt.Errorf("%w: gunzip: %w", errRestoreInput, err)
	}
	if len(raw) > maxBytes {
		return fmt.Errorf("%w: more than %d bytes decompressed", errRestoreInput, maxBytes)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%w: not JSON", errRestoreInput)
	}
	if err := os.WriteFile(filepath.Join(workDir, RestoreStateFile), raw, 0o600); err != nil { // #nosec G703 -- a constant file name under the runner's workdir
		return fmt.Errorf("restore: write state: %w", err)
	}
	return nil
}

// ManagedAddresses returns the count of managed resource instances in out,
// `state list` output: one address per line, data sources (a data. segment
// after the module path) excluded.
func ManagedAddresses(out []byte) int {
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		addr := strings.TrimSpace(sc.Text())
		if addr == "" {
			continue
		}
		if !strings.HasPrefix(resourcePart(addr), "data.") {
			n++
		}
	}
	return n
}

// resourcePart returns addr with its module path (module.<name>[<key>]. …)
// stripped.
func resourcePart(addr string) string {
	for strings.HasPrefix(addr, "module.") {
		rest := addr[len("module."):]
		// The module name, then an optional [key] that may contain dots
		// inside quotes, then ".".
		i := strings.IndexAny(rest, ".[")
		if i < 0 {
			return addr
		}
		rest = rest[i:]
		if strings.HasPrefix(rest, "[") {
			end := closingBracket(rest)
			if end < 0 {
				return addr
			}
			rest = rest[end+1:]
		}
		next, ok := strings.CutPrefix(rest, ".")
		if !ok {
			return addr
		}
		addr = next
	}
	return addr
}

// closingBracket returns the index of the "]" closing s[0] == "[", skipping
// quoted strings; -1 when none.
func closingBracket(s string) int {
	quoted := false
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && quoted:
			i++
		case s[i] == '"':
			quoted = !quoted
		case s[i] == ']' && !quoted:
			return i
		}
	}
	return -1
}
