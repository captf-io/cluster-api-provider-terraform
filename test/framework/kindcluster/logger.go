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

package kindcluster

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"sigs.k8s.io/kind/pkg/log"
)

// writerLogger adapts an io.Writer to kind's log.Logger. Messages at
// V(0) and warnings and errors are written, one line each; higher
// verbosity levels are discarded.
type writerLogger struct {
	// mu serializes writes, since kind logs from several goroutines.
	mu *sync.Mutex
	// w receives the lines.
	w io.Writer
}

// newWriterLogger returns a log.Logger writing to w, or one that discards
// everything when w is nil.
func newWriterLogger(w io.Writer) log.Logger {
	if w == nil {
		w = io.Discard
	}
	return writerLogger{mu: &sync.Mutex{}, w: w}
}

// line writes s to the writer with a trailing newline, adding one only if
// s lacks it.
func (l writerLogger) line(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	_, _ = io.WriteString(l.w, s)
}

// Warn writes message prefixed "warning: ".
func (l writerLogger) Warn(message string) { l.line("warning: " + message) }

// Warnf writes the message built from format and args, prefixed
// "warning: ".
func (l writerLogger) Warnf(format string, args ...any) { l.Warn(fmt.Sprintf(format, args...)) }

// Error writes message prefixed "error: ".
func (l writerLogger) Error(message string) { l.line("error: " + message) }

// Errorf writes the message built from format and args, prefixed
// "error: ".
func (l writerLogger) Errorf(format string, args ...any) { l.Error(fmt.Sprintf(format, args...)) }

// V returns an InfoLogger that writes at level 0 and discards above it.
func (l writerLogger) V(level log.Level) log.InfoLogger {
	return infoLogger{l: l, enabled: level <= 0}
}

// infoLogger is writerLogger's log.InfoLogger.
type infoLogger struct {
	// l is the parent logger.
	l writerLogger
	// enabled reports whether this level writes.
	enabled bool
}

// Info writes message when the level is enabled.
func (i infoLogger) Info(message string) {
	if i.enabled {
		i.l.line(message)
	}
}

// Infof writes the message built from format and args when the level is
// enabled.
func (i infoLogger) Infof(format string, args ...any) {
	if i.enabled {
		i.l.line(fmt.Sprintf(format, args...))
	}
}

// Enabled reports whether this level writes.
func (i infoLogger) Enabled() bool { return i.enabled }
