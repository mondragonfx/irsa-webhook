.PHONY: build push deploy deploy-policy undeploy-policy certs test clean

REGISTRY ?= vultr
IMAGE_NAME ?= irsa-webhook
WEBHOOK_IMAGE ?= $(REGISTRY)/$(IMAGE_NAME):$(TAG)
TAG ?= v0.3.0
NAMESPACE ?= irsa-system

# Build the Go binary
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o webhook ./cmd/

# Build Docker image
docker-build:
	docker build -t $(WEBHOOK_IMAGE) .

# Push Docker image
docker-push:
	docker push $(WEBHOOK_IMAGE)

# Update container image in deploy/webhook.yaml
set-manifest-image:
	sed -i'' -e 's@image: .*irsa-webhook:.*@image: '"$(WEBHOOK_IMAGE)"'@' deploy/webhook.yaml

# Generate TLS certificates
certs:
	chmod +x generate-certs.sh
	./generate-certs.sh

# Deploy the MutatingAdmissionPolicy (Kubernetes >= 1.36, no certs needed)
deploy-policy:
	kubectl apply -f deploy/mutating-admission-policy.yaml

# Remove the MutatingAdmissionPolicy
undeploy-policy:
	kubectl delete -f deploy/mutating-admission-policy.yaml --ignore-not-found

# Deploy the webhook to Kubernetes (older clusters)
deploy:
	@if [ ! -f .ca-bundle.txt ]; then \
		echo "Error: .ca-bundle.txt not found. Run 'make certs' first."; \
		exit 1; \
	fi
	@CA_BUNDLE=$$(cat .ca-bundle.txt) && \
	sed "s|CA_BUNDLE_PLACEHOLDER|$$CA_BUNDLE|g" deploy/webhook.yaml | kubectl apply -f -

# Undeploy from Kubernetes
undeploy:
	kubectl delete -f deploy/webhook.yaml

# View logs
logs:
	kubectl logs -n $(NAMESPACE) -l app=irsa-webhook -f

# Test with example pod
test-example:
	kubectl apply -f examples/example.yaml
	@echo "Waiting for pod to be ready..."
	@kubectl wait --for=condition=Ready pod/example-app --timeout=60s || true
	@echo ""
	@echo "Pod logs:"
	@kubectl logs example-app

# Clean up test resources
clean-example:
	kubectl delete -f examples/example.yaml --ignore-not-found

# Run Go tests
go-test:
	go test -v ./...

# Format Go code
fmt:
	go fmt ./...

# Run Go vet
vet:
	go vet ./...

# Download Go dependencies
deps:
	go mod download
	go mod tidy

# Complete build and deploy pipeline
all: deps build docker-build docker-push set-manifest-image certs deploy

# Check webhook status
status:
	@echo "=== Webhook Deployment ==="
	@kubectl get deployment -n $(NAMESPACE) irsa-webhook
	@echo ""
	@echo "=== Webhook Pods ==="
	@kubectl get pods -n $(NAMESPACE) -l app=irsa-webhook
	@echo ""
	@echo "=== Webhook Service ==="
	@kubectl get svc -n $(NAMESPACE) irsa-webhook
	@echo ""
	@echo "=== MutatingWebhookConfiguration ==="
	@kubectl get mutatingwebhookconfiguration irsa-webhook

# Clean all resources
clean:
	kubectl delete -f deploy/webhook.yaml --ignore-not-found
	kubectl delete namespace $(NAMESPACE) --ignore-not-found
	rm -f webhook

# Restart webhook deployment
restart:
	kubectl rollout restart deployment -n $(NAMESPACE) irsa-webhook
	kubectl rollout status deployment -n $(NAMESPACE) irsa-webhook

# Help
help:
	@echo "Available targets:"
	@echo " build             - Build Go binary"
	@echo " docker-build      - Build Docker image"
	@echo " docker-push       - Push Docker image"
	@echo " set-manifest-image - Update image in deploy/webhook.yaml"
	@echo " certs             - Generate TLS certificates"
	@echo " deploy-policy     - Deploy the MutatingAdmissionPolicy (k8s >= 1.36)"
	@echo " undeploy-policy   - Remove the MutatingAdmissionPolicy"
	@echo " deploy            - Deploy the webhook to Kubernetes"
	@echo " undeploy          - Remove from Kubernetes"
	@echo " logs              - View webhook logs"
	@echo " test-example      - Deploy and test example pod"
	@echo " clean-example     - Remove example pod"
	@echo " go-test           - Run Go tests"
	@echo " fmt               - Format Go code"
	@echo " vet               - Run Go vet"
	@echo " deps              - Download dependencies"
	@echo " all               - Complete build and deploy"
	@echo " status            - Check webhook status"
	@echo " clean             - Remove all resources"
	@echo " restart           - Restart webhook deployment"
	@echo " help              - Show this help"
