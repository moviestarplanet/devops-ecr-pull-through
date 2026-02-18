package main

import (
	"encoding/json"
	"testing"

	v1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func setupServer(t *testing.T, accountID, region, registries string) *server {
	t.Helper()
	t.Setenv("ECR_AWS_ACCOUNT_ID", accountID)
	t.Setenv("ECR_AWS_REGION", region)
	t.Setenv("ECR_REGISTRIES", registries)
	srv, err := newServer()
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return srv
}

func TestMutate(t *testing.T) {
	srv := setupServer(t, "12345", "us-west-2", "ghcr.io,docker.io")

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
		checkMutatePatch(t, srv, pod, map[string]string{
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
		checkMutatePatch(t, srv, pod, map[string]string{
			"/spec/initContainers/0/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/owner/init:1.0",
		})
	})

	t.Run("cross-region ECR rewrite", func(t *testing.T) {
		srv := setupServer(t, "12345", "us-east-1", "12345.dkr.ecr.eu-west-1.amazonaws.com")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app-prefixed", Image: "12345.dkr.ecr.eu-west-1.amazonaws.com/prefix/image:tag"},
					{Name: "app-plain", Image: "12345.dkr.ecr.eu-west-1.amazonaws.com/imagewithoutprefix:tag"},
				},
			},
		}
		checkMutatePatch(t, srv, pod, map[string]string{
			"/spec/containers/0/image": "12345.dkr.ecr.us-east-1.amazonaws.com/prefix/image:tag",
			"/spec/containers/1/image": "12345.dkr.ecr.us-east-1.amazonaws.com/imagewithoutprefix:tag",
		})
	})

	t.Run("cross-account ECR rewrite", func(t *testing.T) {
		srv := setupServer(t, "12345", "us-east-1", "99999.dkr.ecr.eu-west-1.amazonaws.com")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "99999.dkr.ecr.eu-west-1.amazonaws.com/org/image:tag"},
				},
			},
		}
		checkMutatePatch(t, srv, pod, map[string]string{
			"/spec/containers/0/image": "12345.dkr.ecr.us-east-1.amazonaws.com/org/image:tag",
		})
	})

	t.Run("same-region ECR image not patched", func(t *testing.T) {
		srv := setupServer(t, "12345", "us-east-1", "12345.dkr.ecr.eu-west-1.amazonaws.com")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "12345.dkr.ecr.us-east-1.amazonaws.com/msp/comments:us-32"},
				},
			},
		}
		checkMutatePatch(t, srv, pod, map[string]string{}) // already in target region, no patch
	})

	t.Run("third-party ECR account not rewritten", func(t *testing.T) {
		srv := setupServer(t, "12345", "us-east-1", "12345.dkr.ecr.eu-west-1.amazonaws.com")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "99999.dkr.ecr.eu-west-1.amazonaws.com/org/image:tag"},
				},
			},
		}
		checkMutatePatch(t, srv, pod, map[string]string{}) // expect no patch
	})

	t.Run("docker.io images get library normalised", func(t *testing.T) {
		srv := setupServer(t, "12345", "us-west-2", "docker.io")
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
		checkMutatePatch(t, srv, pod, map[string]string{
			"/spec/containers/0/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx",
			"/spec/containers/1/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx",
			"/spec/containers/2/image": "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx",
		})
	})

	t.Run("image already at ecrRegistryHostname is not re-prefixed", func(t *testing.T) {
		srv := setupServer(t, "12345", "us-east-1", "12345.dkr.ecr.us-east-1.amazonaws.com")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app", Image: "12345.dkr.ecr.us-east-1.amazonaws.com/myrepo/myimage:latest"},
				},
			},
		}
		checkMutatePatch(t, srv, pod, map[string]string{}) // already at ecrRegistryHostname, must not double-prefix
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
		checkMutatePatch(t, srv, pod, map[string]string{}) // quay.io not in registry list
	})
}

func TestRewriteImage(t *testing.T) {
	srv := setupServer(t, "12345", "us-west-2", "ghcr.io,docker.io,public.ecr.aws")

	tests := []struct {
		name  string
		image string
		want  string
		ok    bool
	}{
		// Docker Hub normalization
		{"bare image", "nginx", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx", true},
		{"bare image with tag", "nginx:1.25", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx:1.25", true},
		{"implicit docker hub", "owner/image", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/owner/image", true},
		{"explicit docker.io short", "docker.io/nginx", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx", true},
		{"explicit docker.io with library", "docker.io/library/nginx", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx", true},
		{"explicit docker.io with owner", "docker.io/owner/image:1.2", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/owner/image:1.2", true},
		{"docker.io with digest", "docker.io/nginx@sha256:abc", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/library/nginx@sha256:abc", true},
		{"implicit docker hub nested", "a/b/c:tag", "12345.dkr.ecr.us-west-2.amazonaws.com/docker.io/a/b/c:tag", true},

		// Other configured registries
		{"ghcr.io image", "ghcr.io/owner/image:tag", "12345.dkr.ecr.us-west-2.amazonaws.com/ghcr.io/owner/image:tag", true},
		{"public.ecr.aws image", "public.ecr.aws/karpenter/controller:1.8.6", "12345.dkr.ecr.us-west-2.amazonaws.com/public.ecr.aws/karpenter/controller:1.8.6", true},
		{"public.ecr.aws with digest", "public.ecr.aws/karpenter/controller:1.8.6@sha256:dfbaa02d5fad", "12345.dkr.ecr.us-west-2.amazonaws.com/public.ecr.aws/karpenter/controller:1.8.6@sha256:dfbaa02d5fad", true},

		// Unconfigured registry
		{"quay.io not configured", "quay.io/org/repo:tag", "", false},
		{"random registry", "registry.example.com/org/image:tag", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := srv.rewriteImage(tt.image)
			if ok != tt.ok {
				t.Fatalf("rewriteImage(%q) ok = %v, want %v", tt.image, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("rewriteImage(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestRewriteImage_ECR(t *testing.T) {
	srv := setupServer(t, "12345", "us-east-1", "99999.dkr.ecr.eu-west-1.amazonaws.com")

	tests := []struct {
		name  string
		image string
		want  string
		ok    bool
	}{
		{"cross-account rewrite", "99999.dkr.ecr.eu-west-1.amazonaws.com/org/image:tag", "12345.dkr.ecr.us-east-1.amazonaws.com/org/image:tag", true},
		{"different ECR not configured", "88888.dkr.ecr.eu-west-1.amazonaws.com/image:tag", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := srv.rewriteImage(tt.image)
			if ok != tt.ok {
				t.Fatalf("rewriteImage(%q) ok = %v, want %v", tt.image, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("rewriteImage(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func checkMutatePatch(t *testing.T, srv *server, pod *corev1.Pod, want map[string]string) {
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
	mutated, err := srv.mutate(body)
	if err != nil {
		t.Fatalf("mutate error: %v", err)
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
