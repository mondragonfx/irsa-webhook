package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	roleArnAnnotation         = "api.vultr.com/role"
	tokenVolumeName           = "vultr-irsa-token"
	tokenMountPath            = "/var/run/secrets/vultr.com/serviceaccount"
	tokenFileName             = "token"
	tokenAudience             = "vultr"
	envAWSRoleArn             = "AWS_ROLE_ARN"
	envAWSWebIdentityToken    = "AWS_WEB_IDENTITY_TOKEN_FILE"
	envAWSSTSRegionalEndpoint = "AWS_STS_REGIONAL_ENDPOINTS"
)

var (
	tokenExpirationSeconds = int64(86400) // 24 hours
)

type WebhookServer struct {
	client *kubernetes.Clientset
}

// JSONPatch represents a JSON Patch operation
type JSONPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			log.Fatal("KUBECONFIG env var not set")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("Failed to create kubernetes config from KUBECONFIG: %v", err)
		}
		log.Printf("Using kubeconfig from %s", kubeconfig)
	} else {
		log.Printf("Using in-cluster config")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	server := &WebhookServer{
		client: clientset,
	}

	// Setup HTTP handlers
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", server.handleMutate)
	mux.HandleFunc("/health", handleHealth)

	// TLS configuration
	tlsCertPath := getEnv("TLS_CERT_PATH", "/etc/webhook/certs/tls.crt")
	tlsKeyPath := getEnv("TLS_KEY_PATH", "/etc/webhook/certs/tls.key")
	bindAddr := getEnv("BIND_ADDR", "0.0.0.0")
	port := getEnv("PORT", "8443")

	cert, err := tls.LoadX509KeyPair(tlsCertPath, tlsKeyPath)
	if err != nil {
		log.Fatalf("Failed to load TLS certificates: %v", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	httpServer := &http.Server{
		Addr:      fmt.Sprintf("%s:%s", bindAddr, port),
		TLSConfig: tlsConfig,
		Handler:   mux,
	}

	log.Printf("Starting webhook server on port %s", port)
	if err := httpServer.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func (ws *WebhookServer) handleMutate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	// Parse AdmissionReview request
	admissionReview := &admissionv1.AdmissionReview{}
	if err := json.Unmarshal(body, admissionReview); err != nil {
		log.Printf("Failed to unmarshal admission review: %v", err)
		http.Error(w, "Failed to parse admission review", http.StatusBadRequest)
		return
	}

	// Validate request
	if admissionReview.Request == nil {
		log.Printf("Admission review request is nil")
		http.Error(w, "Invalid admission review request", http.StatusBadRequest)
		return
	}

	// Process the request and create response
	admissionResponse := ws.mutate(admissionReview.Request)

	// Create response AdmissionReview
	responseAdmissionReview := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: admissionResponse,
	}
	responseAdmissionReview.Response.UID = admissionReview.Request.UID

	// Marshal and send response
	responseBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		log.Printf("Failed to marshal admission response: %v", err)
		http.Error(w, "Failed to create response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBytes)
}

func (ws *WebhookServer) mutate(request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// Create base response
	response := &admissionv1.AdmissionResponse{
		Allowed: true,
	}

	// Only handle Pod objects
	if request.Kind.Kind != "Pod" {
		log.Printf("Skipping non-Pod object: %s", request.Kind.Kind)
		return response
	}

	// Parse Pod from request
	pod := &corev1.Pod{}
	if err := json.Unmarshal(request.Object.Raw, pod); err != nil {
		log.Printf("Failed to unmarshal pod: %v", err)
		response.Allowed = false
		response.Result = &metav1.Status{
			Message: fmt.Sprintf("Failed to parse pod: %v", err),
		}
		return response
	}

	log.Printf("Processing pod: %s/%s, ServiceAccount: %s",
		request.Namespace, pod.Name, pod.Spec.ServiceAccountName)

	// Get ServiceAccount name (default to "default" if not specified)
	serviceAccountName := pod.Spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = "default"
	}

	// Fetch ServiceAccount from Kubernetes API
	serviceAccount, err := ws.client.CoreV1().ServiceAccounts(request.Namespace).Get(
		context.Background(),
		serviceAccountName,
		metav1.GetOptions{},
	)
	if err != nil {
		log.Printf("Failed to fetch ServiceAccount %s/%s: %v",
			request.Namespace, serviceAccountName, err)
		// Don't block pod creation if we can't fetch the SA
		return response
	}

	// Check for role ARN annotation
	roleArn, exists := serviceAccount.Annotations[roleArnAnnotation]
	if !exists || roleArn == "" {
		log.Printf("ServiceAccount %s/%s does not have %s annotation, skipping mutation",
			request.Namespace, serviceAccountName, roleArnAnnotation)
		return response
	}

	log.Printf("Found role ARN annotation: %s", roleArn)

	// Generate JSON patches to mutate the pod
	patches, err := ws.generatePatches(pod, roleArn)
	if err != nil {
		log.Printf("Failed to generate patches: %v", err)
		response.Allowed = false
		response.Result = &metav1.Status{
			Message: fmt.Sprintf("Failed to generate patches: %v", err),
		}
		return response
	}

	// Marshal patches to JSON
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		log.Printf("Failed to marshal patches: %v", err)
		response.Allowed = false
		response.Result = &metav1.Status{
			Message: fmt.Sprintf("Failed to marshal patches: %v", err),
		}
		return response
	}

	response.Patch = patchBytes
	patchType := admissionv1.PatchTypeJSONPatch
	response.PatchType = &patchType

	log.Printf("Successfully generated %d patches for pod %s/%s",
		len(patches), request.Namespace, pod.Name)

	return response
}

func (ws *WebhookServer) generatePatches(pod *corev1.Pod, roleArn string) ([]JSONPatch, error) {
	var patches []JSONPatch

	// 1. Add projected volume for service account token
	tokenVolume := corev1.Volume{
		Name: tokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          tokenAudience,
							ExpirationSeconds: &tokenExpirationSeconds,
							Path:              tokenFileName,
						},
					},
				},
			},
		},
	}

	// Check if volumes array exists
	if pod.Spec.Volumes == nil {
		// Initialize volumes array
		patches = append(patches, JSONPatch{
			Op:    "add",
			Path:  "/spec/volumes",
			Value: []corev1.Volume{tokenVolume},
		})
	} else {
		// Append to existing volumes
		patches = append(patches, JSONPatch{
			Op:    "add",
			Path:  "/spec/volumes/-",
			Value: tokenVolume,
		})
	}

	// 2. Process each container
	for i := range pod.Spec.Containers {
		containerPatches := ws.generateContainerPatches(i, roleArn, &pod.Spec.Containers[i])
		patches = append(patches, containerPatches...)
	}

	// 3. Process init containers if they exist
	for i := range pod.Spec.InitContainers {
		containerPatches := ws.generateContainerPatches(i, roleArn, &pod.Spec.InitContainers[i])
		// Adjust paths for init containers
		for j := range containerPatches {
			containerPatches[j].Path = "/spec/initContainers" +
				containerPatches[j].Path[len("/spec/containers"):]
		}
		patches = append(patches, containerPatches...)
	}

	return patches, nil
}

func (ws *WebhookServer) generateContainerPatches(index int, roleArn string, container *corev1.Container) []JSONPatch {
	var patches []JSONPatch
	basePath := fmt.Sprintf("/spec/containers/%d", index)

	// Add volume mount
	volumeMount := corev1.VolumeMount{
		Name:      tokenVolumeName,
		MountPath: tokenMountPath,
		ReadOnly:  true,
	}

	if container.VolumeMounts == nil {
		// Initialize volume mounts array
		patches = append(patches, JSONPatch{
			Op:    "add",
			Path:  basePath + "/volumeMounts",
			Value: []corev1.VolumeMount{volumeMount},
		})
	} else {
		// Append to existing volume mounts
		patches = append(patches, JSONPatch{
			Op:    "add",
			Path:  basePath + "/volumeMounts/-",
			Value: volumeMount,
		})
	}

	// Get STS endpoint from environment (set in deployment)
	stsEndpoint := getEnv("STS_ENDPOINT", "https://api.vultr.com/v2/assumed-roles/compatibility/aws/sts")

	// Add environment variables
	envVars := []corev1.EnvVar{
		{
			Name:  envAWSRoleArn,
			Value: roleArn,
		},
		{
			Name:  envAWSWebIdentityToken,
			Value: fmt.Sprintf("%s/%s", tokenMountPath, tokenFileName),
		},
		{
			Name:  envAWSSTSRegionalEndpoint,
			Value: "regional",
		},
		{
			Name:  "AWS_ENDPOINT_URL_STS",
			Value: stsEndpoint,
		},
	}

	if container.Env == nil {
		// Initialize env array
		patches = append(patches, JSONPatch{
			Op:    "add",
			Path:  basePath + "/env",
			Value: envVars,
		})
	} else {
		// Append to existing env vars
		for _, envVar := range envVars {
			patches = append(patches, JSONPatch{
				Op:    "add",
				Path:  basePath + "/env/-",
				Value: envVar,
			})
		}
	}

	return patches
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
