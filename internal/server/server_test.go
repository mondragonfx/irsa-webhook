package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vultr/irsa-webhook/internal/mutate"
)

const testRole = "775a6be6-45cd-4f19-94f5-6e4f96f093ec"

func newServer(t *testing.T, objects ...runtime.Object) *Server {
	t.Helper()
	return New(fake.NewClientset(objects...), mutate.New(mutate.Config{}))
}

func annotatedSA(ns, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace:   ns,
		Name:        name,
		Annotations: map[string]string{mutate.RoleAnnotation: testRole},
	}}
}

func podRequest(t *testing.T, ns string, pod *corev1.Pod) *admissionv1.AdmissionRequest {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return &admissionv1.AdmissionRequest{
		UID:       types.UID("req-1"),
		Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
		Namespace: ns,
		Name:      pod.Name,
		Object:    runtime.RawExtension{Raw: raw},
	}
}

func simplePod(name, sa string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			ServiceAccountName: sa,
			Containers:         []corev1.Container{{Name: "app"}},
		},
	}
}

func TestMutateAnnotatedServiceAccount(t *testing.T) {
	s := newServer(t, annotatedSA("default", "irsa-sa"))
	resp := s.Mutate(context.Background(), podRequest(t, "default", simplePod("p", "irsa-sa")))
	if !resp.Allowed {
		t.Fatalf("expected allowed, got %v", resp.Result)
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatalf("expected JSONPatch patch type, got %v", resp.PatchType)
	}
	var patches []mutate.JSONPatch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 3 {
		t.Fatalf("expected 3 patches for a bare pod, got %d: %v", len(patches), patches)
	}
}

func TestMutateDefaultServiceAccountFallback(t *testing.T) {
	s := newServer(t, annotatedSA("default", "default"))
	resp := s.Mutate(context.Background(), podRequest(t, "default", simplePod("p", "")))
	if !resp.Allowed || resp.Patch == nil {
		t.Fatalf("expected patch via default SA, got allowed=%v patch=%s", resp.Allowed, resp.Patch)
	}
}

func TestMutateUnannotatedServiceAccount(t *testing.T) {
	plain := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "plain"}}
	s := newServer(t, plain)
	resp := s.Mutate(context.Background(), podRequest(t, "default", simplePod("p", "plain")))
	if !resp.Allowed || resp.Patch != nil {
		t.Fatalf("expected unchanged admission, got allowed=%v patch=%s", resp.Allowed, resp.Patch)
	}
}

func TestMutateMissingServiceAccountAdmitsUnchanged(t *testing.T) {
	s := newServer(t)
	resp := s.Mutate(context.Background(), podRequest(t, "default", simplePod("p", "ghost")))
	if !resp.Allowed || resp.Patch != nil {
		t.Fatalf("expected unchanged admission, got allowed=%v patch=%s", resp.Allowed, resp.Patch)
	}
}

func TestMutateIgnoresNonPods(t *testing.T) {
	s := newServer(t, annotatedSA("default", "irsa-sa"))
	req := podRequest(t, "default", simplePod("p", "irsa-sa"))
	req.Kind.Kind = "Deployment"
	resp := s.Mutate(context.Background(), req)
	if !resp.Allowed || resp.Patch != nil {
		t.Fatalf("expected non-pod to pass through, got allowed=%v patch=%s", resp.Allowed, resp.Patch)
	}
}

func TestMutateAlreadyInjectedPodHasNoPatch(t *testing.T) {
	s := newServer(t, annotatedSA("default", "irsa-sa"))
	pod := simplePod("p", "irsa-sa")
	// Run once, apply the outcome by hand, and run again.
	pod.Spec.Volumes = []corev1.Volume{{Name: mutate.TokenVolumeName}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: mutate.TokenVolumeName, MountPath: mutate.TokenMountPath}}
	for _, name := range []string{"VULTR_ROLE_ID", "VULTR_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_STS_REGIONAL_ENDPOINTS", "AWS_ENDPOINT_URL_STS"} {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: name, Value: "set"})
	}
	resp := s.Mutate(context.Background(), podRequest(t, "default", pod))
	if !resp.Allowed || resp.Patch != nil {
		t.Fatalf("expected no patch on reinvocation, got %s", resp.Patch)
	}
}

func TestMutateRejectsUnparseablePod(t *testing.T) {
	s := newServer(t, annotatedSA("default", "irsa-sa"))
	req := podRequest(t, "default", simplePod("p", "irsa-sa"))
	req.Object.Raw = []byte(`{"spec": "not-an-object"}`)
	resp := s.Mutate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected unparseable pod to be denied")
	}
}

func TestHandleMutateHTTP(t *testing.T) {
	s := newServer(t, annotatedSA("default", "irsa-sa"))
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request:  podRequest(t, "default", simplePod("p", "irsa-sa")),
	}
	body, _ := json.Marshal(review)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Response == nil || out.Response.UID != "req-1" || !out.Response.Allowed || out.Response.Patch == nil {
		t.Fatalf("unexpected response: %s", rec.Body)
	}
}

func TestHandleMutateBadRequests(t *testing.T) {
	s := newServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mutate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader([]byte("{"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader([]byte("{}"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing request: status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health: status %d", rec.Code)
	}
}
