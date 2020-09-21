/*
Copyright 2020 Google LLC

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

package deployment

import (
	"testing"

	"knative.dev/pkg/configmap"
	. "knative.dev/pkg/reconciler/testing"

	// Fake injection informers
	_ "github.com/google/knative-gcp/pkg/reconciler/testing"
	_ "knative.dev/pkg/client/injection/kube/informers/apps/v1/deployment/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/core/v1/secret/fake"
)

func TestNew(t *testing.T) {
	ctx, _ := SetupFakeContext(t)

	ctor := NewConstructor()

	c := ctor(ctx, configmap.NewStaticWatcher())

	if c == nil {
		t.Fatal("Expected NewController to return a non-nil value")
	}
}
