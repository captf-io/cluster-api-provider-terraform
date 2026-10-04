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

package framework

// Every version, download and image the test environment uses is pinned
// here, and nothing else in the module hardcodes one. versions_test.go
// checks that each pin is well formed.
//
// How to bump (resolve every value; never pin a tag alone):
//
//   - kind and the node image: bump sigs.k8s.io/kind in test/go.mod and
//     KindVersion together, then take the v1.36.x image line, digest
//     included, from that kind release's notes
//     (gh release view <kind-version> -R kubernetes-sigs/kind). The node
//     image tracks the product's k8s.io/client-go minor (v0.36 -> v1.36).
//   - cluster-api: set CAPIVersion, download each CAPI release asset below
//     from .../releases/download/<CAPIVersion>/ into
//     `mktemp -d ~/tmp/captf-pins-XXXX`, and record `sha256sum` of each.
//     Keep it equal to the clusterctl the Makefile pins.
//   - cert-manager: take CertManagerDefaultVersion from
//     sigs.k8s.io/cluster-api@<CAPIVersion>/cmd/clusterctl/client/config/cert_manager_client.go,
//     then download and hash its cert-manager.yaml the same way.
//   - images: resolve each digest from its readable ref with
//     `GOTOOLCHAIN=local go run github.com/google/go-containerregistry/cmd/crane@v0.20.8 digest <ref>`.
//     The digest is the multi-arch index digest.

// KindVersion is the sigs.k8s.io/kind library version in test/go.mod. The
// node image below comes from this release's supported image list.
const KindVersion = "v0.33.0"

// KubernetesVersion is the Kubernetes version of the management cluster.
// Its minor matches the product's k8s.io/client-go (v0.36).
const KubernetesVersion = "v1.36.4"

// KindNodeImage is the kind node image for KubernetesVersion, as listed in
// the kind KindVersion release notes, pinned by its multi-arch index
// digest. The tag is for readers; the digest is what gets pulled.
const KindNodeImage = "docker.io/kindest/node:" + KubernetesVersion +
	"@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"

// CAPIVersion is the cluster-api release installed into the management
// cluster: core, kubeadm bootstrap and kubeadm control plane. It matches
// the clusterctl version the Makefile pins.
const CAPIVersion = "v1.14.2"

// CertManagerVersion is the cert-manager release clusterctl CAPIVersion
// installs by default (CertManagerDefaultVersion in clusterctl's
// cert_manager_client.go).
const CertManagerVersion = "v1.21.1"

// capiReleaseURL is the download prefix of the CAPIVersion release assets.
const capiReleaseURL = "https://github.com/kubernetes-sigs/cluster-api/releases/download/" + CAPIVersion + "/"

// Artifact is one downloadable file, pinned by the sha256 of its content.
// Callers verify the download against SHA256 before using it.
type Artifact struct {
	// Name is the file name, which is also the name a clusterctl local
	// repository expects (for example "core-components.yaml").
	Name string
	// URL is the https download URL.
	URL string
	// SHA256 is the lowercase hex sha256 of the file's content.
	SHA256 string
}

// The CAPI release assets a clusterctl local repository needs. Each
// provider directory holds its components file plus the release's single
// metadata.yaml, which covers core, bootstrap and control plane alike.
var (
	// CAPICoreComponents is the core provider's (cluster-api) components.
	CAPICoreComponents = Artifact{
		Name:   "core-components.yaml",
		URL:    capiReleaseURL + "core-components.yaml",
		SHA256: "b2fff42cb5e35440ed963a463c5ab128004481b5a4336ea0005812be2ff7e2a8",
	}
	// CAPIBootstrapComponents is the kubeadm bootstrap provider's components.
	CAPIBootstrapComponents = Artifact{
		Name:   "bootstrap-components.yaml",
		URL:    capiReleaseURL + "bootstrap-components.yaml",
		SHA256: "2a2d24f83244a6dae60e35d9e72e93c6ac3c209eedc467184737bb8eecfd63bb",
	}
	// CAPIControlPlaneComponents is the kubeadm control-plane provider's
	// components.
	CAPIControlPlaneComponents = Artifact{
		Name:   "control-plane-components.yaml",
		URL:    capiReleaseURL + "control-plane-components.yaml",
		SHA256: "7aa827b43eee898d8597bb39b7db5cf755c872c7a4da68b05cc3a9f35c459c93",
	}
	// CAPIMetadata is the release's metadata.yaml, shared by all three
	// providers.
	CAPIMetadata = Artifact{
		Name:   "metadata.yaml",
		URL:    capiReleaseURL + "metadata.yaml",
		SHA256: "c470906f551ac3e3e9aedc9a3e733b5a5994428693df43e95f65ea67c035f9fe",
	}
	// CertManagerManifest is cert-manager CertManagerVersion's release
	// manifest, which clusterctl init applies before the providers.
	CertManagerManifest = Artifact{
		Name:   "cert-manager.yaml",
		URL:    "https://github.com/cert-manager/cert-manager/releases/download/" + CertManagerVersion + "/cert-manager.yaml",
		SHA256: "5f6a499b8c1857d57f560f536e0dcc830914b45c420899fe7ad0692c8624e408",
	}
)

// CAPIArtifacts returns a fresh slice of the four CAPI release assets:
// core, bootstrap and control-plane components, then metadata.
func CAPIArtifacts() []Artifact {
	return []Artifact{CAPICoreComponents, CAPIBootstrapComponents, CAPIControlPlaneComponents, CAPIMetadata}
}

// Artifacts returns a fresh slice of every pinned download: the CAPI
// release assets followed by the cert-manager manifest.
func Artifacts() []Artifact {
	return append(CAPIArtifacts(), CertManagerManifest)
}

// CAPIProvider is one cluster-api provider as a clusterctl local repository
// and clusterctl.yaml describe it.
type CAPIProvider struct {
	// Name is the clusterctl provider name ("cluster-api" or "kubeadm"),
	// as passed to `clusterctl init --core|--bootstrap|--control-plane`.
	Name string
	// Type is the clusterctl provider type for clusterctl.yaml's
	// providers list: CoreProvider, BootstrapProvider or
	// ControlPlaneProvider.
	Type string
	// Label is the provider's directory in a local repository; files go
	// in <repository>/<Label>/<Version>/.
	Label string
	// Version is the provider's release, CAPIVersion for all three.
	Version string
	// Components is the provider's components file.
	Components Artifact
	// Metadata is the provider's metadata.yaml.
	Metadata Artifact
}

// CAPIProviders returns a fresh slice of the three CAPI providers the
// environment installs, in clusterctl init order: core, bootstrap, then
// control plane.
func CAPIProviders() []CAPIProvider {
	return []CAPIProvider{
		{Name: "cluster-api", Type: "CoreProvider", Label: "cluster-api", Version: CAPIVersion, Components: CAPICoreComponents, Metadata: CAPIMetadata},
		{Name: "kubeadm", Type: "BootstrapProvider", Label: "bootstrap-kubeadm", Version: CAPIVersion, Components: CAPIBootstrapComponents, Metadata: CAPIMetadata},
		{Name: "kubeadm", Type: "ControlPlaneProvider", Label: "control-plane-kubeadm", Version: CAPIVersion, Components: CAPIControlPlaneComponents, Metadata: CAPIMetadata},
	}
}

// NoopRole is the CAPTF object kind a noop module image serves.
type NoopRole string

// The three noop module roles, one image repository each.
const (
	// RoleCluster is the TerraformCluster module (ghcr.io/captf-io/noop-cluster).
	RoleCluster NoopRole = "cluster"
	// RoleMachine is the TerraformMachine module (ghcr.io/captf-io/noop-machine).
	RoleMachine NoopRole = "machine"
	// RoleMachinePool is the TerraformMachinePool module (ghcr.io/captf-io/noop-machinepool).
	RoleMachinePool NoopRole = "machinepool"
)

// NoopRuntime is the infrastructure-as-code runtime a noop module image
// is built for.
type NoopRuntime string

// The two runtimes, one image tag each.
const (
	// RuntimeTerraform is HashiCorp Terraform (tag edge-terraform).
	RuntimeTerraform NoopRuntime = "terraform"
	// RuntimeOpenTofu is OpenTofu (tag edge-opentofu).
	RuntimeOpenTofu NoopRuntime = "opentofu"
)

// NoopImage is one published noop module image.
type NoopImage struct {
	// Role is the CAPTF object kind the module serves.
	Role NoopRole
	// Runtime is the runtime the image is built for.
	Runtime NoopRuntime
	// Repository is the image repository, ghcr.io/captf-io/noop-<role>.
	Repository string
	// Ref is the readable tag reference, <Repository>:edge-<runtime>.
	// Tags move: pull Pinned, then tag the result as Ref.
	Ref string
	// Digest is the multi-arch index digest, "sha256:<64 hex>".
	Digest string
}

// Pinned returns the image's pull reference, Repository@Digest, which
// resolves to Digest whatever the tag points to now. It carries no tag, so
// every engine accepts it.
func (i NoopImage) Pinned() string {
	return i.Repository + "@" + i.Digest
}

// noopImage builds the NoopImage for role and runtime with digest, and
// returns it with Repository and Ref derived from role and runtime.
func noopImage(role NoopRole, runtime NoopRuntime, digest string) NoopImage {
	repo := "ghcr.io/captf-io/noop-" + string(role)
	return NoopImage{
		Role:       role,
		Runtime:    runtime,
		Repository: repo,
		Ref:        repo + ":edge-" + string(runtime),
		Digest:     digest,
	}
}

// NoopImages returns a fresh slice of the six published noop module
// images, every role for each runtime, Terraform first.
func NoopImages() []NoopImage {
	return []NoopImage{
		noopImage(RoleCluster, RuntimeTerraform, "sha256:25663d2744fb10440867592fbad8c3dd360e36ed8ce130a32be737c7c6443454"),
		noopImage(RoleMachine, RuntimeTerraform, "sha256:29e883366e0a96b3eb861a5c3ea45627df41fa1e0907e1b053181d94f4beb62b"),
		noopImage(RoleMachinePool, RuntimeTerraform, "sha256:95863e471beafeddea4ced20794fca68316674d340adade6329e484fcae8b6d8"),
		noopImage(RoleCluster, RuntimeOpenTofu, "sha256:533d4a41e079319899d932fbdbd0bd137a88adc4379a4f88e1afc10f972901b9"),
		noopImage(RoleMachine, RuntimeOpenTofu, "sha256:426769a12c6e59cd0bdaed212e5245659d05478a05a5c3a660cab13225b81769"),
		noopImage(RoleMachinePool, RuntimeOpenTofu, "sha256:294d4aeb0e841f170b91b436058faa7302ba7e1b53936fd686544a30ed977db3"),
	}
}

// NoopImageFor returns the noop image for role and runtime, and reports
// with ok whether one is pinned.
func NoopImageFor(role NoopRole, runtime NoopRuntime) (img NoopImage, ok bool) {
	for _, i := range NoopImages() {
		if i.Role == role && i.Runtime == runtime {
			return i, true
		}
	}
	return NoopImage{}, false
}
