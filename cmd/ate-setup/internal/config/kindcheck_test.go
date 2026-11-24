// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.ate.dev/substrate/cmd/ate-setup/internal/images"
)

const kindcheckKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1"}
users:
- name: u
  user: {token: t}
contexts:
- name: kind-test
  context: {cluster: c, user: u}
current-context: kind-test
`

// Without docker on PATH, a build from source fails the check and a
// pre-built install passes it.
func TestCheckKindClusterDocker(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(kindcheckKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir) // no docker here

	build := &Config{Kubeconfig: kubeconfig, Context: "kind-test"}
	if err := build.CheckKindCluster(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot access Docker") {
		t.Errorf("build from source: got %v, want the Docker error", err)
	}

	prebuilt := &Config{Kubeconfig: kubeconfig, Context: "kind-test",
		Images: images.Source{Repo: "registry.example.com/substrate", Tag: "v1"}}
	if err := prebuilt.CheckKindCluster(context.Background()); err != nil {
		t.Errorf("pre-built: %v", err)
	}
}
