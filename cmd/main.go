package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	v1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "ECR Pull-through webhook %q", html.EscapeString(r.URL.Path))
}

var config *Config
var ecrRegistryHostname string

func initGlobals(c *Config) {
	for i, reg := range c.Registries {
		c.Registries[i] = strings.TrimRight(reg, "/") + "/"
	}
	config = c
	ecrRegistryHostname = fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com/", c.AwsAccountID, c.AwsRegion)
}

// isEcrRegistry reports whether the given registry hostname belongs to an ECR endpoint.
func isEcrRegistry(registry string) bool {
	return strings.Contains(registry, ".dkr.ecr.")
}

// normalizeDockerHubImage ensures Docker Hub images have an explicit
// docker.io/library/ or docker.io/ prefix. Non-Docker-Hub images are
// returned unchanged.
func normalizeDockerHubImage(image string) string {
	host, path, hasSlash := strings.Cut(image, "/")
	if !hasSlash {
		return "docker.io/library/" + image
	}
	if strings.Contains(host, ".") || strings.Contains(host, ":") {
		if host != "docker.io" {
			return image
		}
		if !strings.Contains(path, "/") {
			// docker.io/nginx -> docker.io/library/nginx
			return "docker.io/library/" + path
		}
		return image
	}
	// No registry specified -> Docker Hub implicit
	return "docker.io/" + image
}

// rewriteImage normalizes the image, checks whether it belongs to the given
// registry, and returns the pull-through cache path. Returns ("", false) when
// the image does not match the registry.
func rewriteImage(image, registry string) (string, bool) {
	normalized := image
	if registry == "docker.io/" {
		normalized = normalizeDockerHubImage(image)
	}
	if !strings.HasPrefix(normalized, registry) {
		return "", false
	}
	if isEcrRegistry(registry) {
		return ecrRegistryHostname + normalized[strings.Index(normalized, "/")+1:], true
	}
	return ecrRegistryHostname + normalized, true
}

func handleMutate(w http.ResponseWriter, r *http.Request) {

	// read the body / request
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// mutate the request
	mutated, err := actuallyMutate(body)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// and write it back
	w.WriteHeader(http.StatusOK)
	w.Write(mutated)
}

func actuallyMutate(body []byte) ([]byte, error) {
	// unmarshal request into AdmissionReview struct
	admReview := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(body, &admReview); err != nil {
		return nil, fmt.Errorf("unmarshaling request failed with %s", err)
	}

	var err error
	var pod *corev1.Pod

	responseBody := []byte{}
	ar := admReview.Request
	resp := v1beta1.AdmissionResponse{}

	if ar != nil {

		// get the Pod object and unmarshal it into its struct, if we cannot, we might as well stop here
		if err := json.Unmarshal(ar.Object.Raw, &pod); err != nil {
			return nil, fmt.Errorf("unable unmarshal pod json object %v", err)
		}
		log.Printf("Received request to mutate pod %s:%s", pod.Namespace, pod.ObjectMeta.GenerateName)
		// set response options
		resp.Allowed = true
		resp.UID = ar.UID
		pT := v1beta1.PatchTypeJSONPatch
		resp.PatchType = &pT

		// the actual mutation is done by a string in JSONPatch style, i.e. we don't _actually_ modify the object, but
		// tell K8S how it should modifiy it
		p := []map[string]string{}

		addPatchForImage := func(image, path string) {
			if strings.HasPrefix(image, ecrRegistryHostname) {
				return
			}
			for _, reg := range config.RegistryList() {
				if newImage, ok := rewriteImage(image, reg); ok {
					p = append(p, map[string]string{"op": "replace", "path": path, "value": newImage})
					log.Printf("Created patch for image %s on pod %s:%s, with %s", image, pod.Namespace, pod.ObjectMeta.GenerateName, newImage)
					return
				}
			}
		}

		// Containers
		for i, container := range pod.Spec.Containers {
			addPatchForImage(container.Image, fmt.Sprintf("/spec/containers/%d/image", i))
		}
		// InitContainers
		for i, initcontainer := range pod.Spec.InitContainers {
			addPatchForImage(initcontainer.Image, fmt.Sprintf("/spec/initContainers/%d/image", i))
		}
		// EphemeralContainers
		for i, ephemeralcontainer := range pod.Spec.EphemeralContainers {
			addPatchForImage(ephemeralcontainer.Image, fmt.Sprintf("/spec/ephemeralContainers/%d/image", i))
		}

		// parse the []map into JSON
		resp.Patch, _ = json.Marshal(p)

		// Success, of course ;)
		resp.Result = &metav1.Status{
			Status: "Success",
		}

		admReview.Response = &resp
		// back into JSON so we can return the finished AdmissionReview w/ Response directly
		// w/o needing to convert things in the http handler
		responseBody, err = json.Marshal(admReview)

		if err != nil {
			return nil, err // untested section
		}
		log.Printf("Successfully mutated pod %s:%s", pod.Namespace, pod.ObjectMeta.Name)
	}

	return responseBody, nil
}

func main() {
	var err error
	config, err = ReadConf("/etc/ecr-pull-through/registries.yaml")
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}
	initGlobals(config)

	mux := http.NewServeMux()

	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/mutate", handleMutate)

	s := &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	// Check for TLS certificate and key files
	_, certErr := os.Stat("/etc/webhook/certs/tls.crt")
	_, keyErr := os.Stat("/etc/webhook/certs/tls.key")

	if os.IsNotExist(certErr) || os.IsNotExist(keyErr) {
		log.Println("Starting server without TLS...")
		log.Fatal(s.ListenAndServe())
	} else {
		log.Println("Starting server with TLS...")
		log.Fatal(s.ListenAndServeTLS("/etc/webhook/certs/tls.crt", "/etc/webhook/certs/tls.key"))
	}
}
