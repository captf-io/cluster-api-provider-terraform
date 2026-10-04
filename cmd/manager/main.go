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

package main

// The blank imports load every client-go auth plugin, for out-of-cluster
// runs, and register the json logging format.
import (
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/component-base/cli"
	_ "k8s.io/component-base/logs/json/register"

	"github.com/captf-io/cluster-api-provider-terraform/cmd/manager/app"
)

// main builds the manager's cobra command and hands it to
// k8s.io/component-base/cli.Run, which parses flags, initializes and
// flushes logging, executes the command and prints or logs any error
// exactly once; it exits the process with the resulting status code. It
// takes no parameters: all configuration comes from the command-line flags
// and environment app.Run reads.
func main() {
	command := app.NewManagerCommand()
	code := cli.Run(command)
	os.Exit(code)
}
