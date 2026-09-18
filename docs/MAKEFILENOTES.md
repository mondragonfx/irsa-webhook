To use the Makefile, simply run `make` followed by the target name:

```bash
# View all available commands
make help

# Common commands:

# Download Go dependencies
make deps

# Build the Go binary
make build

# Build Docker image
make docker-build

# Build and push Docker image
make docker-build docker-push

# Deploy the MutatingAdmissionPolicy (k8s >= 1.36, no certificates needed)
make deploy-policy

# Generate TLS certificates (webhook mode)
make certs

# Deploy to Kubernetes
make deploy

# View webhook logs
make logs

# Check webhook status
make status

# Test with example pod
make test-example

# Restart webhook
make restart

# Complete pipeline (build, push, certs, deploy)
make all

# Clean up everything
make clean
```

**Example workflow:**

```bash
# 1. First, download dependencies
make deps

# 2. Build locally to test
make build

# 3. Build Docker image (update IMAGE_NAME in Makefile first)
make docker-build

# 4. Push to registry
make docker-push

# 5. Generate certificates
make certs

# 6. Deploy to cluster
make deploy

# 7. Check status
make status

# 8. Watch logs
make logs
```

**Customizing:**

You can override variables:

```bash
# Use custom image name
make docker-build IMAGE_NAME=myregistry.com/irsa-webhook IMAGE_TAG=v1.0.0

# Use custom namespace
make deploy NAMESPACE=custom-namespace
```