package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// setupServer prepares config and starts an httptest server with the mutate handler.
func setupServer() (*httptest.Server, func()) {
	initGlobals(&Config{
		Registries:   []string{"ghcr.io", "docker.io"},
		AwsAccountID: "99999",
		AwsRegion:    "eu-central-1",
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", handleMutate)
	srv := httptest.NewServer(mux)

	return srv, func() { srv.Close() }
}

// doMutate posts the given pod to the test server and returns the parsed patches.
func doMutate(t *testing.T, srvURL string, pod *corev1.Pod) []map[string]string {
	t.Helper()
	podJSON, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}

	admReq := &v1beta1.AdmissionRequest{UID: "u1", Object: runtime.RawExtension{Raw: podJSON}}
	admReview := &v1beta1.AdmissionReview{Request: admReq}
	body, err := json.Marshal(admReview)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}

	resp, err := http.Post(srvURL+"/mutate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post mutate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}

	out := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}

	if out.Response == nil {
		t.Fatalf("nil response")
	}

	if !out.Response.Allowed {
		t.Fatalf("mutation not allowed")
	}

	var patches []map[string]string
	if err := json.Unmarshal(out.Response.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}

	return patches
}

func findPatchValue(patches []map[string]string, path string) (string, bool) {
	for _, p := range patches {
		if p["path"] == path {
			return p["value"], true
		}
	}
	return "", false
}

func TestMutateHandler_Containers(t *testing.T) {
	srv, closeFn := setupServer()
	defer closeFn()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "int-pod-c", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "c1", Image: "nginx:latest"},
				{Name: "c2", Image: "ghcr.io/owner/app:1.0"},
			},
		},
	}

	patches := doMutate(t, srv.URL, pod)

	want0 := "99999.dkr.ecr.eu-central-1.amazonaws.com/docker.io/library/nginx:latest"
	if got, ok := findPatchValue(patches, "/spec/containers/0/image"); !ok {
		t.Fatalf("missing patch for containers/0")
	} else if got != want0 {
		t.Fatalf("containers/0 got=%q want=%q", got, want0)
	}

	want1 := "99999.dkr.ecr.eu-central-1.amazonaws.com/ghcr.io/owner/app:1.0"
	if got, ok := findPatchValue(patches, "/spec/containers/1/image"); !ok {
		t.Fatalf("missing patch for containers/1")
	} else if got != want1 {
		t.Fatalf("containers/1 got=%q want=%q", got, want1)
	}
}

func TestMutateHandler_InitContainers(t *testing.T) {
	srv, closeFn := setupServer()
	defer closeFn()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "int-pod-i", Namespace: "default"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init1", Image: "owner/init:0.1"}},
		},
	}

	patches := doMutate(t, srv.URL, pod)

	wantInit := "99999.dkr.ecr.eu-central-1.amazonaws.com/docker.io/owner/init:0.1"
	if got, ok := findPatchValue(patches, "/spec/initContainers/0/image"); !ok {
		t.Fatalf("missing patch for initContainers/0")
	} else if got != wantInit {
		t.Fatalf("initContainers/0 got=%q want=%q", got, wantInit)
	}
}

func TestMutateHandler_UnconfiguredRegistryIgnored(t *testing.T) {
	srv, closeFn := setupServer()
	defer closeFn()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "int-pod-e", Namespace: "default"},
		Spec: corev1.PodSpec{
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "e1", Image: "quay.io/org/repo:tag"}}},
		},
	}

	patches := doMutate(t, srv.URL, pod)
	if _, ok := findPatchValue(patches, "/spec/ephemeralContainers/0/image"); ok {
		t.Fatalf("ephemeral patch found but should not be present")
	}
}
