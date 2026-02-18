package main

import (
	"encoding/json"
	"testing"

	v1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestActuallyMutate(t *testing.T) {
	initGlobals(&Config{
		Registries:   []string{"ghcr.io", "docker.io"},
		AwsAccountID: "12345",
		AwsRegion:    "us-west-2",
	})

	t.Run("containers", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c-nginx", Image: "nginx"},
					{Name: "c-ghcr", Image: "ghcr.io/owner/image:tag"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{
			"/spec/containers/0/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx",
			"/spec/containers/1/image": "12345.dkr.ecr.us-west-2.amazonaws.com/ghcr.io/owner/image:tag",
		})
	})

	t.Run("initContainers", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "init-1", Image: "owner/init:1.0"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{
			"/spec/initContainers/0/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/owner/init:1.0",
		})
	})

	t.Run("cross-region ECR rewrite", func(t *testing.T) {
		initGlobals(&Config{
			Registries:   []string{"12345.dkr.ecr.eu-west-1.amazonaws.com"},
			AwsAccountID: "12345",
			AwsRegion:    "us-east-1",
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app-prefixed", Image: "12345.dkr.ecr.eu-west-1.amazonaws.com/prefix/image:tag"},
					{Name: "app-plain", Image: "12345.dkr.ecr.eu-west-1.amazonaws.com/imagewithoutprefix:tag"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{
			"/spec/containers/0/image": "12345.dkr.ecr.us-east-1.amazonaws.com/prefix/image:tag",
			"/spec/containers/1/image": "12345.dkr.ecr.us-east-1.amazonaws.com/imagewithoutprefix:tag",
		})
	})

	t.Run("cross-account ECR rewrite", func(t *testing.T) {
		initGlobals(&Config{
			Registries:   []string{"99999.dkr.ecr.eu-west-1.amazonaws.com"},
			AwsAccountID: "12345",
			AwsRegion:    "us-east-1",
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "99999.dkr.ecr.eu-west-1.amazonaws.com/org/image:tag"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{
			"/spec/containers/0/image": "12345.dkr.ecr.us-east-1.amazonaws.com/org/image:tag",
		})
	})

	t.Run("same-region ECR image not patched", func(t *testing.T) {
		initGlobals(&Config{
			Registries:   []string{"12345.dkr.ecr.eu-west-1.amazonaws.com"},
			AwsAccountID: "12345",
			AwsRegion:    "us-east-1",
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "12345.dkr.ecr.us-east-1.amazonaws.com/msp/comments:us-32"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{}) // already in target region, no patch
	})

	t.Run("third-party ECR account not rewritten", func(t *testing.T) {
		initGlobals(&Config{
			Registries:   []string{"12345.dkr.ecr.eu-west-1.amazonaws.com"},
			AwsAccountID: "12345",
			AwsRegion:    "us-east-1",
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "99999.dkr.ecr.eu-west-1.amazonaws.com/org/image:tag"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{}) // expect no patch
	})

	t.Run("docker.io images get library normalised", func(t *testing.T) {
		initGlobals(&Config{
			Registries:   []string{"docker.io"},
			AwsAccountID: "12345",
			AwsRegion:    "us-west-2",
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "implicit", Image: "nginx"},
					{Name: "explicit", Image: "docker.io/nginx"},
					{Name: "explicit-library", Image: "docker.io/library/nginx"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{
			"/spec/containers/0/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx",
			"/spec/containers/1/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx",
			"/spec/containers/2/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx",
		})
	})

	t.Run("image already at ecrRegistryHostname is not re-prefixed", func(t *testing.T) {
		initGlobals(&Config{
			Registries:   []string{"12345.dkr.ecr.us-east-1.amazonaws.com"},
			AwsAccountID: "12345",
			AwsRegion:    "us-east-1",
		})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "12345.dkr.ecr.us-east-1.amazonaws.com/myrepo/myimage:latest"},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{}) // already at ecrRegistryHostname, must not double-prefix
	})

	t.Run("unconfigured registry not patched", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				EphemeralContainers: []corev1.EphemeralContainer{
					{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "e-1", Image: "quay.io/org/repo:tag"}},
				},
			},
		}
		checkMutatePatch(t, pod, map[string]string{}) // quay.io not in registry list
	})
}

func checkMutatePatch(t *testing.T, pod *corev1.Pod, want map[string]string) {
	t.Helper()
	podJSON, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	admReq := &v1beta1.AdmissionRequest{
		UID:    "test-uid",
		Object: runtime.RawExtension{Raw: podJSON},
	}
	admReview := &v1beta1.AdmissionReview{Request: admReq}
	body, err := json.Marshal(admReview)
	if err != nil {
		t.Fatalf("marshal admissionreview: %v", err)
	}
	mutated, err := actuallyMutate(body)
	if err != nil {
		t.Fatalf("actuallyMutate error: %v", err)
	}
	out := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(mutated, &out); err != nil {
		t.Fatalf("unmarshal mutated review: %v", err)
	}
	if out.Response == nil {
		t.Fatalf("response is nil")
	}
	var patches []map[string]string
	if err := json.Unmarshal(out.Response.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	got := map[string]string{}
	for _, p := range patches {
		if path, ok := p["path"]; ok {
			got[path] = p["value"]
		}
	}
	for k, v := range want {
		if gotV, ok := got[k]; !ok {
			t.Errorf("missing patch for %s", k)
		} else if gotV != v {
			t.Errorf("patch for %s: got %q want %q", k, gotV, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected patch for %s", k)
		}
	}
}

func TestNormalizeDockerHubImage(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"nginx", "docker.io/library/nginx"},
		{"image:1.2.3", "docker.io/library/image:1.2.3"},
		{"owner/image", "docker.io/owner/image"},
		{"owner/image:tag", "docker.io/owner/image:tag"},
		{"docker.io/nginx@sha256:abc", "docker.io/library/nginx@sha256:abc"},
		{"docker.io/library/nginx", "docker.io/library/nginx"},
		{"docker.io/owner/image:1.2", "docker.io/owner/image:1.2"},
		{"a/b/c:tag", "docker.io/a/b/c:tag"},

		// non-docker registries -> returned unchanged
		{"ghcr.io/owner/image:tag", "ghcr.io/owner/image:tag"},
		{"quay.io/org/repo", "quay.io/org/repo"},
		{"registry.example.com/org/image:tag", "registry.example.com/org/image:tag"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeDockerHubImage(tt.name)
			if got != tt.want {
				t.Fatalf("normalizeDockerHubImage(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
