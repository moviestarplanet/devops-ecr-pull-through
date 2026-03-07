package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const dockerHubRegistry = "docker.io/"

type server struct {
	registries          []string
	ecrRegistryHostname string
}

type CertReloader struct {
	mu                sync.RWMutex
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

	cr.mu.RLock()
	if cr.cachedCert != nil && !stat.ModTime().After(cr.cachedCertModTime) {
		cert := cr.cachedCert
		cr.mu.RUnlock()
		return cert, nil
	}
	cr.mu.RUnlock()

	cr.mu.Lock()
	defer cr.mu.Unlock()
	// Re-check under write lock in case another goroutine already reloaded.
	if cr.cachedCert != nil && !stat.ModTime().After(cr.cachedCertModTime) {
		return cr.cachedCert, nil
	}
	pair, err := tls.LoadX509KeyPair(cr.certPath, cr.keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed loading tls key pair: %w", err)
	}
	cr.cachedCert = &pair
	cr.cachedCertModTime = stat.ModTime()
	slog.Info("TLS certificate loaded")
	return cr.cachedCert, nil
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "ECR Pull-through webhook %q", html.EscapeString(r.URL.Path))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
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
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			slog.Error("failed to read request body", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	mutated, err := s.mutate(body)
	if err != nil {
		slog.Error("failed to mutate request", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(mutated)
}

func (s *server) mutate(body []byte) ([]byte, error) {
	admReview := admissionv1.AdmissionReview{}
	if err := json.Unmarshal(body, &admReview); err != nil {
		return nil, fmt.Errorf("unmarshaling request failed with %s", err)
	}

	var pod *corev1.Pod

	responseBody := []byte{}
	ar := admReview.Request
	resp := admissionv1.AdmissionResponse{}

	if ar != nil {
		if err := json.Unmarshal(ar.Object.Raw, &pod); err != nil {
			return nil, fmt.Errorf("unable unmarshal pod json object %v", err)
		}
		slog.Info("received mutation request", "namespace", pod.Namespace, "pod", pod.ObjectMeta.GenerateName)

		resp.Allowed = true
		resp.UID = ar.UID
		pT := admissionv1.PatchTypeJSONPatch
		resp.PatchType = &pT

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

		for i, container := range pod.Spec.Containers {
			addPatchForImage(container.Image, fmt.Sprintf("/spec/containers/%d/image", i))
		}
		for i, initcontainer := range pod.Spec.InitContainers {
			addPatchForImage(initcontainer.Image, fmt.Sprintf("/spec/initContainers/%d/image", i))
		}
		for i, ephemeralcontainer := range pod.Spec.EphemeralContainers {
			addPatchForImage(ephemeralcontainer.Image, fmt.Sprintf("/spec/ephemeralContainers/%d/image", i))
		}

		var err error
		resp.Patch, err = json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal patch: %w", err)
		}

		resp.Result = &metav1.Status{
			Status: "Success",
		}

		admReview.Response = &resp
		admReview.TypeMeta = metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		}

		responseBody, err = json.Marshal(admReview)
		if err != nil {
			return nil, err
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
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/ready", handleHealth)
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

	if !os.IsNotExist(certErr) && !os.IsNotExist(keyErr) {
		reloader := &CertReloader{certPath: certPath, keyPath: keyPath}
		s.TLSConfig = &tls.Config{
			GetCertificate: reloader.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
	}

	go func() {
		var err error
		if s.TLSConfig != nil {
			err = s.ListenAndServeTLS("", "")
		} else {
			err = s.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
		os.Exit(1)
	}
}
