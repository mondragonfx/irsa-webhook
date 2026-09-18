// Package server implements the admission HTTP endpoints for the IRSA webhook.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vultr/irsa-webhook/internal/mutate"
)

// maxBodyBytes caps the AdmissionReview we are willing to read. Pods are
// small; anything larger is not a request the API server would send.
const maxBodyBytes = 4 << 20

// Server serves /mutate and /health.
type Server struct {
	client  kubernetes.Interface
	mutator *mutate.Mutator
}

// New returns a Server backed by client for ServiceAccount lookups.
func New(client kubernetes.Interface, mutator *mutate.Mutator) *Server {
	return &Server{client: client, mutator: mutator}
}

// Handler returns the http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", s.handleMutate)
	mux.HandleFunc("/health", handleHealth)
	return mux
}

func (s *Server) handleMutate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		log.Printf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}

	review := &admissionv1.AdmissionReview{}
	if err := json.Unmarshal(body, review); err != nil {
		log.Printf("Failed to unmarshal admission review: %v", err)
		http.Error(w, "Failed to parse admission review", http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		http.Error(w, "Invalid admission review request", http.StatusBadRequest)
		return
	}

	response := s.Mutate(r.Context(), review.Request)
	response.UID = review.Request.UID

	out, err := json.Marshal(&admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Response: response,
	})
	if err != nil {
		log.Printf("Failed to marshal admission response: %v", err)
		http.Error(w, "Failed to create response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// Mutate decides whether the Pod in request belongs to an annotated
// ServiceAccount and, if so, returns a JSON patch response.
func (s *Server) Mutate(ctx context.Context, request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	allowed := &admissionv1.AdmissionResponse{Allowed: true}

	if request.Kind.Kind != "Pod" {
		return allowed
	}

	pod := &corev1.Pod{}
	if err := json.Unmarshal(request.Object.Raw, pod); err != nil {
		return denied(fmt.Sprintf("failed to parse pod: %v", err))
	}

	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}

	sa, err := s.client.CoreV1().ServiceAccounts(request.Namespace).Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		// Without the ServiceAccount there is nothing to decide. Admit the Pod
		// unchanged rather than blocking unrelated workloads on an API hiccup;
		// the Pod would fail to start anyway if the ServiceAccount is missing.
		log.Printf("ServiceAccount %s/%s lookup failed, admitting pod %q unchanged: %v",
			request.Namespace, saName, podName(request, pod), err)
		return allowed
	}

	role := sa.Annotations[mutate.RoleAnnotation]
	if role == "" {
		return allowed
	}

	patches := s.mutator.Patches(pod, role)
	if len(patches) == 0 {
		return allowed
	}

	raw, err := json.Marshal(patches)
	if err != nil {
		return denied(fmt.Sprintf("failed to marshal patches: %v", err))
	}

	log.Printf("Injected role %s into pod %s/%s (%d patches)", role, request.Namespace, podName(request, pod), len(patches))

	patchType := admissionv1.PatchTypeJSONPatch
	allowed.Patch = raw
	allowed.PatchType = &patchType
	return allowed
}

func denied(msg string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result:  &metav1.Status{Message: msg},
	}
}

// podName prefers the request name, falling back to generateName for Pods
// created by controllers, whose metadata.name is empty at admission time.
func podName(request *admissionv1.AdmissionRequest, pod *corev1.Pod) string {
	if pod.Name != "" {
		return pod.Name
	}
	if request.Name != "" {
		return request.Name
	}
	return pod.GenerateName + "*"
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
