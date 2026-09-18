// Command irsa-webhook is a mutating admission webhook that injects Vultr IAM
// role credentials into Pods whose ServiceAccount carries the
// api.vultr.com/role annotation.
//
// On Kubernetes 1.36 and newer, prefer deploy/mutating-admission-policy.yaml,
// which performs the same injection inside the API server with no webhook,
// TLS, or Deployment at all. This binary exists for older clusters.
package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vultr/irsa-webhook/internal/mutate"
	"github.com/vultr/irsa-webhook/internal/server"
)

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			log.Fatal("Not running in-cluster and KUBECONFIG env var not set")
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

	mutator := mutate.New(mutate.Config{
		STSEndpoint:            os.Getenv("STS_ENDPOINT"),
		TokenExpirationSeconds: envInt64("TOKEN_EXPIRATION_SECONDS", mutate.DefaultTokenExpirationSeconds),
	})
	srv := server.New(clientset, mutator)

	tlsCertPath := getEnv("TLS_CERT_PATH", "/etc/webhook/certs/tls.crt")
	tlsKeyPath := getEnv("TLS_KEY_PATH", "/etc/webhook/certs/tls.key")
	bindAddr := getEnv("BIND_ADDR", "0.0.0.0")
	port := getEnv("PORT", "8443")

	cert, err := tls.LoadX509KeyPair(tlsCertPath, tlsKeyPath)
	if err != nil {
		log.Fatalf("Failed to load TLS certificates: %v", err)
	}

	httpServer := &http.Server{
		Addr: fmt.Sprintf("%s:%s", bindAddr, port),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	log.Printf("Starting webhook server on %s", httpServer.Addr)
	if err := httpServer.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func envInt64(key string, defaultValue int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultValue
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		log.Fatalf("%s must be a positive integer, got %q", key, raw)
	}
	return v
}
