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

package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/spf13/cobra"

	"github.com/captf-io/cluster-api-provider-terraform/internal/lint/image"
)

// newImageCommand returns the "image" subcommand: it pulls the image
// reference named by its single positional argument, using remoteOpts to
// extend every go-containerregistry call (nil in production, an in-memory
// registry's options in tests), lints it against the contract for --role,
// prints the report, and exits with the code the report implies
// (ExitFindings, ExitUnparsable or ExitUsage).
func newImageCommand(remoteOpts []remote.Option) *cobra.Command {
	var f commonFlags
	var o image.Options
	cmd := &cobra.Command{
		Use:           "image <image-ref>",
		Short:         "Lint a built source image against the image contract",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, ref, err := validateCommon(cmd, "image reference", args, f)
			if err != nil {
				return err
			}
			o.Role, o.Contract = role, f.contract
			o.PlatformSet = cmd.Flags().Changed("platform")
			o.Remote = remoteOpts
			res, err := image.Lint(cmd.Context(), ref, o)
			switch {
			case errors.Is(err, image.ErrReference):
				return usageErr(cmd, err.Error())
			case err != nil:
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
				return &exitCodeErr{Code: ExitUnparsable}
			}
			info := res.Info
			if !f.json {
				fmt.Fprintf(cmd.OutOrStdout(), "image %s (%s) platforms %s\n", info.Ref, info.Digest, strings.Join(info.Platforms, ", "))
			}
			return finish(cmd, f, jsonReport{Image: &info, Contract: f.contract, Role: string(role)}, res.Report)
		},
	}
	bindCommonFlags(cmd, &f)
	fs := cmd.Flags()
	fs.StringVar(&o.Platform, "platform", image.DefaultPlatform, "the platform to check in a multi-platform image")
	fs.BoolVar(&o.AllPlatforms, "all-platforms", false, "check every platform of a multi-platform image")
	fs.BoolVar(&o.Insecure, "insecure", false, "allow a plain-HTTP registry")
	return cmd
}
