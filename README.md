# IRSA for Vultr Kubernetes

IAM Roles for Service Accounts (IRSA) lets a pod assume a Vultr IAM role using
its projected ServiceAccount token, with no static API keys in the cluster.
Annotate a ServiceAccount with a Vultr role ID and every pod that uses it
receives a short-lived token, signed by the cluster and trusted by Vultr IAM,
that the application exchanges for a Vultr API session.

The consumer is any application that calls the Vultr API: the cluster
autoscaler, the cloud controller manager, or your own code using govultr.

This repository ships two ways to perform that injection:

| Mode | Kubernetes | What runs in your cluster | TLS certificates |
|---|---|---|---|
| **MutatingAdmissionPolicy** (recommended) | 1.36+ (1.34/1.35 with the feature gate) | Nothing. Two cluster-scoped policy objects evaluated inside the API server. | None |
| **Webhook** | 1.20+ | A two-replica Deployment, Service and MutatingWebhookConfiguration | Required, see below |

Both modes produce identical pods.

## Features

- Gives pods a projected ServiceAccount token, with a dedicated audience, that
  Vultr IAM accepts in exchange for a role session
- Injects `VULTR_ROLE_ID` and the token path so applications know what to
  exchange; no static API keys anywhere in the cluster
- Handles containers and init containers
- Idempotent: safe under webhook reinvocation and alongside sidecar injectors
- Opt-out per namespace with a single label
- No dependencies beyond the Kubernetes API

## How It Works

When a pod is created, the policy or webhook:

1. Looks up the pod's ServiceAccount (`default` when none is set)
2. Checks for the `api.vultr.com/role` annotation
3. If present, mutates the pod to add:
   - **Volume:** a projected ServiceAccount token with audience `vultr`
   - **Volume mounts:** the token at `/var/run/secrets/vultr.com/serviceaccount/token`
   - **Environment variables** on every container and init container:
     - `VULTR_ROLE_ID`: the value of the annotation
     - `VULTR_WEB_IDENTITY_TOKEN_FILE`: path to the projected token
     - `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_STS_REGIONAL_ENDPOINTS`,
       `AWS_ENDPOINT_URL_STS`: kept for consumers written against the
       STS-compatible endpoint; `AWS_ROLE_ARN` carries the same role ID

The application then exchanges the token for a Vultr API session; see
[Using the role from your application](#using-the-role-from-your-application).

Pods in `kube-system`, `kube-node-lease`, `kube-public` and the webhook's own
namespace are never mutated. Label any other namespace `irsa-webhook=disabled`
to exclude it.

## Prerequisites

- A Vultr IAM role and an OIDC issuer registered for your cluster (see
  [Registering your cluster](#registering-your-cluster))
- `kubectl` configured to access your cluster
- For the webhook mode only: OpenSSL for certificate generation

## Install: MutatingAdmissionPolicy (recommended)

```bash
kubectl apply -f deploy/mutating-admission-policy.yaml
```

That is the whole install. The policy runs inside the API server, so there is
no image to pull, no Deployment to run, and no certificate to manage.

On Kubernetes 1.34 or 1.35 the feature is beta and off by default. Start the
API server with `--feature-gates=MutatingAdmissionPolicy=true` and
`--runtime-config=admissionregistration.k8s.io/v1beta1=true`, and change the
`apiVersion` in the manifest to `admissionregistration.k8s.io/v1beta1`.

## Install: Webhook (Kubernetes older than 1.36)

### 1. Build the image (optional)

Release images are published as `vultr/irsa-webhook:<tag>`. To build your own:

```bash
docker build -t your-registry/irsa-webhook:latest .
docker push your-registry/irsa-webhook:latest
```

Update the image in `deploy/webhook.yaml`, or run
`make set-manifest-image WEBHOOK_IMAGE=your-registry/irsa-webhook:latest`.

### 2. Generate TLS certificates

The API server calls the webhook over TLS, so it needs a serving certificate
and the CA that signed it:

```bash
./generate-certs.sh
```

This creates a self-signed CA and certificate, stores them in the
`irsa-webhook-certs` Secret, and records the CA bundle for the next step.
Automatic certificate bootstrapping and rotation inside the webhook is
planned; until then rerun this script to rotate.

### 3. Deploy

```bash
make deploy
```

This substitutes the CA bundle into `deploy/webhook.yaml` and applies it,
creating the `irsa-system` namespace, RBAC, a two-replica Deployment with a
PodDisruptionBudget, a Service, and the MutatingWebhookConfiguration.

### 4. Verify

```bash
kubectl get pods -n irsa-system
kubectl logs -n irsa-system -l app=irsa-webhook
```

## Usage

### Annotate a ServiceAccount

Set the `api.vultr.com/role` annotation to the ID of the Vultr IAM role the
pods should assume:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: my-app
  namespace: default
  annotations:
    api.vultr.com/role: "775a6be6-45cd-4f19-94f5-6e4f96f093ec"
```

The annotation is read when a pod is **created**. Pods that already exist
when the annotation is added must be recreated.

### Deploy a pod

Any pod using that ServiceAccount receives the configuration automatically:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
  namespace: default
spec:
  serviceAccountName: my-app
  containers:
  - name: app
    image: curlimages/curl:latest
    command: ["sleep", "3600"]
```

### Verify injection

```bash
kubectl exec my-app -- env | grep VULTR_
kubectl exec my-app -- ls -la /var/run/secrets/vultr.com/serviceaccount
```

Then exchange the token as shown in the next section; a `201` with a
`session_token` means the cluster's issuer is registered and the role trusts it.

## Using the role from your application

The projected token is a JWT with audience `vultr`. Exchange it at the
native assume-role endpoint and use the returned `session_token` as a Vultr
API bearer token for the lifetime of the session:

```bash
TOKEN=$(cat "$VULTR_WEB_IDENTITY_TOKEN_FILE")
curl -s -X POST https://api.vultr.com/v2/assumed-roles/assume \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"role_id\": \"$VULTR_ROLE_ID\", \"session_name\": \"$HOSTNAME\", \"auth_method\": \"oidc\", \"duration\": 3600}"
# -> {"session_token": "...", "expires_at": "2026-09-18 15:42:19", ...}
```

The session is scoped to the role's policies and expires after `duration`
seconds (at most the role's `max_session_duration`). Re-read the token file
and repeat the exchange before `expires_at`; the kubelet rotates the
projected token automatically, so never cache it beyond a single exchange.

In Go, wrap that in an `oauth2.TokenSource` and hand it to govultr. The
cluster-autoscaler Vultr provider is the reference implementation of this
pattern: it refreshes the session five minutes before expiry and falls back
to a static token when `VULTR_ROLE_ID` is unset.

Trust is granted on the Vultr side with a role trust of type
`TemporaryAssumption` for your cluster's OIDC issuer. Note that role trusts
currently support IP, time-of-day and expiry conditions only; a trust to an
issuer applies to every ServiceAccount in that cluster.

## Configuration

### Webhook environment variables


### Failure policy

Both manifests fail closed (`failurePolicy: Fail`). A pod whose ServiceAccount
asked for credentials is rejected, with a clear error, rather than silently
started without them. System namespaces and the webhook's own namespace are
excluded so a broken webhook can always be repaired. Set
`failurePolicy: Ignore` in `deploy/webhook.yaml` only if you would rather
have pods start without credentials during a webhook outage.

## Security Considerations

1. **Least privilege**: the webhook ServiceAccount can only read ServiceAccounts
2. **TLS**: the webhook serves TLS 1.2 or newer; the policy mode has no network path at all
3. **Non-root**: the container runs as user 65532 with a read-only root filesystem and no capabilities
4. **Idempotent mutation**: rerunning the injection never duplicates volumes or variables

## Troubleshooting

See [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md). The short version:

```bash
# Is the ServiceAccount annotated?
kubectl get sa <name> -o jsonpath='{.metadata.annotations.api\.vultr\.com/role}'

# Policy mode: is the policy bound and free of errors?
kubectl get mutatingadmissionpolicy irsa.vultr.com -o yaml

# Webhook mode: is it healthy?
kubectl get mutatingwebhookconfiguration irsa-webhook -o yaml
kubectl logs -n irsa-system -l app=irsa-webhook
```

## Development

```bash
go test ./...
go build -o webhook ./cmd/
```

The injection logic lives in `internal/mutate` and is covered by unit tests,
including idempotency and reinvocation cases. The admission handler lives in
`internal/server` and is tested against a fake Kubernetes client.

## Registering your cluster

Before pods can assume roles, Vultr must trust your cluster's ServiceAccount
token signer. Register an OIDC issuer at `https://api.vultr.com/v2/oidc/issuer`:

  - For VKE clusters:
  ```
  {
    "source": "vke",
    "source_id": "a070d34b-8380-441a-8fb4-d5a9c4001226" #This is the id of your VKE cluster
  }
  ```
  - For NON VKE clusters:
  ```
  {
    "source": "external",
    "uri": "https://64c243de-eb0b-4084-93ae-6c386bef8978.vultr-k8s.com:6443/openid/v1/jwks",
    "kid": "Sf4VzjgTmm_pW91u5qZypZWwiac9_boRFPC5vEmuhCQ",
    "kty": "RSA",
    "n": "3nhZuoDdSSr6OvdnxfOiJKZoC3kcnuEqbJyxXx0ULZLld3rxOmY8w1cuVjNIOaQsZZzQ6qeR7Z315L-Cdi19SLJRcdPf4d0Nezj9pmE_C0VjyNa8w0ZeF23xgiSnE4-ZamLdPtmxWXGhyyBSc_3CRBo-yFdAYJrsmXT1jjm_DOFpI3ZnKqeK7zmG9pRK-OaXfIXw_PEAZ3scflUkv1tE_j21YnFYd8BSM_He_V4Wx3MRFEBqr9-NbVegsEaQsZU63G_BCxEQXHXXM1YJ9ubE29jvMUrSHNFrgLrAjQhXrwu-PpEU1ROwbG4G0FaWkxEzC2K2_gqVC-Q4g-eEYS73UQ",
    "e": "AQAB",
    "alg": "RS256",
    "use": "sig"
  }
  ```
- From this point your cluster's ServiceAccount tokens are trusted by Vultr
  IAM. Grant a role trust to the issuer (see
  [Using the role from your application](#using-the-role-from-your-application)),
  install the webhook or policy from this repo, and annotate a ServiceAccount
  to start receiving injected pods.
