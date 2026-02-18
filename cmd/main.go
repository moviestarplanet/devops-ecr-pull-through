package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	v1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const dockerHubRegistry = "docker.io/"

type server struct {
	registries          []string
	ecrRegistryHostname string
}

type CertReloader struct {
	certPath          string
	keyPath           string
	cachedCert        *tls.Certificate
	cachedCertModTime time.Time
}

func newServer() (*server, error) {
	accountID := os.Getenv("ECR_AWS_ACCOUNT_ID")
	if accountID == "" {
		return nil, fmt.Errorf("ECR_AWS_ACCOUNT_ID is required")
	}

	region := os.Getenv("ECR_AWS_REGION")
	if region == "" {
		return nil, fmt.Errorf("ECR_AWS_REGION is required")
	}

	var registries []string
	if raw := os.Getenv("ECR_REGISTRIES"); raw != "" {
		for r := range strings.SplitSeq(raw, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				registries = append(registries, strings.TrimRight(r, "/")+"/")
			}
		}
	}
	if len(registries) == 0 {
		registries = []string{dockerHubRegistry}
	}

	return &server{
		registries:          registries,
		ecrRegistryHostname: fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com/", accountID, region),
	}, nil
}

func (cr *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	stat, err := os.Stat(cr.certPath)
	if err != nil {
		return nil, fmt.Errorf("failed checking cert file modification time: %w", err)
	}
	if cr.cachedCert == nil || stat.ModTime().After(cr.cachedCertModTime) {
		pair, err := tls.LoadX509KeyPair(cr.certPath, cr.keyPath)
		if err != nil {
			return nil, fmt.Errorf("failed loading tls key pair: %w", err)
		}
		cr.cachedCert = &pair
		cr.cachedCertModTime = stat.ModTime()
		slog.Info("TLS certificate loaded")
	}
	return cr.cachedCert, nil
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "ECR Pull-through webhook %q", html.EscapeString(r.URL.Path))
}

// isEcrRegistry reports whether the given registry hostname belongs to an ECR endpoint.
func isEcrRegistry(registry string) bool {
	return strings.Contains(registry, ".dkr.ecr.")
}

// rewriteImage normalizes the image, checks whether its registry is in the
// configured list, and returns the pull-through cache path. Returns ("", false)
// when the image's registry is not configured.
func (s *server) rewriteImage(image string) (string, bool) {
	var registry, path string
	i := strings.IndexByte(image, '/') + 1
	if i == 0 {
		// bare image: "nginx" → docker.io/, library/nginx
		registry = dockerHubRegistry
		path = "library/" + image
	} else {
		registry = image[:i]
		path = image[i:]
		if !strings.Contains(registry, ".") && !strings.Contains(registry, ":") {
			// no registry specified, implicit Docker Hub: "owner/image" → docker.io/, owner/image
			registry = dockerHubRegistry
			path = image
		} else if registry == dockerHubRegistry && !strings.Contains(path, "/") {
			// docker.io/nginx → docker.io/, library/nginx
			path = "library/" + path
		}
	}

	if !slices.Contains(s.registries, registry) {
		return "", false
	}
	if isEcrRegistry(registry) {
		return s.ecrRegistryHostname + path, true
	}
	return s.ecrRegistryHostname + registry + path, true
}

func (s *server) handleMutate(w http.ResponseWriter, r *http.Request) {

	// read the body / request
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		slog.Error("failed to read request body", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// mutate the request
	mutated, err := s.mutate(body)
	if err != nil {
		slog.Error("failed to mutate request", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// and write it back
	w.WriteHeader(http.StatusOK)
	w.Write(mutated)
}

func (s *server) mutate(body []byte) ([]byte, error) {
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
		slog.Info("received mutation request", "namespace", pod.Namespace, "pod", pod.ObjectMeta.GenerateName)
		// set response options
		resp.Allowed = true
		resp.UID = ar.UID
		pT := v1beta1.PatchTypeJSONPatch
		resp.PatchType = &pT

		// the actual mutation is done by a string in JSONPatch style, i.e. we don't _actually_ modify the object, but
		// tell K8S how it should modifiy it
		p := []map[string]string{}

		addPatchForImage := func(image, path string) {
			if strings.HasPrefix(image, s.ecrRegistryHostname) {
				return
			}
			if newImage, ok := s.rewriteImage(image); ok {
				p = append(p, map[string]string{"op": "replace", "path": path, "value": newImage})
				slog.Info("patched image", "namespace", pod.Namespace, "pod", pod.ObjectMeta.GenerateName, "original", image, "new", newImage)
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
		slog.Info("mutation complete", "namespace", pod.Namespace, "pod", pod.ObjectMeta.Name)
	}

	return responseBody, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	srv, err := newServer()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/mutate", srv.handleMutate)

	s := &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	// Check for TLS certificate and key files
	certPath := "/etc/webhook/certs/tls.crt"
	keyPath := "/etc/webhook/certs/tls.key"
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)

	if os.IsNotExist(certErr) || os.IsNotExist(keyErr) {
		slog.Info("starting server without TLS")
		log.Fatal(s.ListenAndServe())
	} else {
		reloader := &CertReloader{certPath: certPath, keyPath: keyPath}
		s.TLSConfig = &tls.Config{
			GetCertificate: reloader.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
		slog.Info("starting server with dynamic TLS reloading")
		log.Fatal(s.ListenAndServeTLS("", ""))
	}
}
