# Troubleshooting Guide

Most of this guide concerns the **webhook** mode. In the **MutatingAdmissionPolicy**
mode there is no Deployment, Service, TLS or RBAC to debug; check the policy and
binding instead:

```bash
kubectl get mutatingadmissionpolicy irsa.vultr.com -o yaml
kubectl get mutatingadmissionpolicybinding irsa.vultr.com -o yaml
# Type-check errors surface in .status.typeChecking; runtime errors are
# returned on the pod create call because the policy fails closed.
```

## Common Issues and Solutions

### 1. Webhook Not Responding

**Symptoms:**
- Pods fail to create with timeout errors
- Events show webhook timeout
- `kubectl get pods` hangs

**Diagnosis:**
```bash
# Check webhook pod status
kubectl get pods -n irsa-system

# View webhook logs
kubectl logs -n irsa-system -l app=irsa-webhook

# Check webhook service
kubectl get svc -n irsa-system irsa-webhook
kubectl get endpoints -n irsa-system irsa-webhook
```

**Solutions:**

1. **Webhook pods not running:**
   ```bash
   kubectl describe pods -n irsa-system -l app=irsa-webhook
   # Fix image pull issues, resource constraints, etc.
   ```

2. **TLS certificate issues:**
   ```bash
   # Regenerate certificates
   ./generate-certs.sh
   kubectl rollout restart deployment -n irsa-system irsa-webhook
   ```

3. **Service not routing correctly:**
   ```bash
   # Check if service selectors match pod labels
   kubectl get svc -n irsa-system irsa-webhook -o yaml
   kubectl get pods -n irsa-system -l app=irsa-webhook --show-labels
   ```

### 2. Pods Not Being Mutated

**Symptoms:**
- Pods create successfully but don't have injected configuration
- Environment variables missing
- Volume not mounted

**Diagnosis:**
```bash
# Check if ServiceAccount has annotation
kubectl get sa <service-account-name> -o yaml | grep api.vultr.com/role

# Check webhook configuration
kubectl get mutatingwebhookconfiguration irsa-webhook -o yaml

# View webhook logs for the specific pod creation
kubectl logs -n irsa-system -l app=irsa-webhook --tail=100
```

**Solutions:**

1. **ServiceAccount annotation missing:**
   ```bash
   kubectl annotate sa <service-account-name> \
     api.vultr.com/role="775a6be6-45cd-4f19-94f5-6e4f96f093ec"
   ```

2. **Namespace excluded from webhook:**
   Check the `namespaceSelector` in the MutatingWebhookConfiguration:
   ```bash
   kubectl get mutatingwebhookconfiguration irsa-webhook -o yaml
   ```

3. **Webhook not receiving requests:**
   ```bash
   # Check webhook logs for incoming requests
   kubectl logs -n irsa-system -l app=irsa-webhook --tail=50

   # Verify webhook configuration matches service
   kubectl get mutatingwebhookconfiguration irsa-webhook -o jsonpath='{.webhooks[0].clientConfig}'
   ```

### 3. RBAC Permission Errors

**Symptoms:**
- Webhook logs show "forbidden" or "unauthorized" errors
- Error fetching ServiceAccounts

**Diagnosis:**
```bash
# Check webhook ServiceAccount permissions
kubectl auth can-i get serviceaccounts \
  --as=system:serviceaccount:irsa-system:irsa-webhook \
  --all-namespaces

# View RBAC resources
kubectl get clusterrole irsa-webhook -o yaml
kubectl get clusterrolebinding irsa-webhook -o yaml
```

**Solutions:**

1. **Missing RBAC permissions:**
   ```bash
   # Reapply RBAC configuration
   make deploy
   ```

2. **ServiceAccount not bound to role:**
   ```bash
   kubectl get clusterrolebinding irsa-webhook -o yaml
   # Verify subjects include the correct ServiceAccount
   ```

### 4. TLS/Certificate Issues

**Symptoms:**
- "x509: certificate signed by unknown authority"
- "TLS handshake error"
- Webhook returns 401 or 403

**Diagnosis:**
```bash
# Check certificate in secret
kubectl get secret -n irsa-system irsa-webhook-certs -o yaml

# Verify CA bundle in webhook config
kubectl get mutatingwebhookconfiguration irsa-webhook \
  -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | base64 -d
```

**Solutions:**

1. **Regenerate certificates:**
   ```bash
   ./generate-certs.sh
   ```

2. **Manually update CA bundle:**
   ```bash
   CA_BUNDLE=$(kubectl get secret -n irsa-system irsa-webhook-certs \
     -o jsonpath='{.data.ca\.crt}')

   kubectl patch mutatingwebhookconfiguration irsa-webhook \
     --type='json' \
     -p="[{'op': 'replace', 'path': '/webhooks/0/clientConfig/caBundle', 'value':'${CA_BUNDLE}'}]"
   ```

3. **Verify certificate SANs:**
   ```bash
   kubectl get secret -n irsa-system irsa-webhook-certs \
     -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -text -noout
   ```

### 5. Credential Exchange Failures

**Symptoms:**
- "Invalid API token" (401) when exchanging the projected token
- "No trust relationship exists between this role and OIDC issuer" (403)
- AWS SDKs report "Unable to locate credentials" or an STS error

**Diagnosis:**
```bash
# Check injected environment variables
kubectl exec <pod-name> -- env | grep VULTR_

# Verify token file exists
kubectl exec <pod-name> -- ls -la /var/run/secrets/vultr.com/serviceaccount/

# Exchange the token directly (see "Using the role from your application"
# in the README for the full command); the response tells you which of the
# cases below applies
kubectl exec <pod-name> -- sh -c \
  'curl -s -X POST https://api.vultr.com/v2/assumed-roles/assume \
     -H "Authorization: Bearer $(cat $VULTR_WEB_IDENTITY_TOKEN_FILE)" \
     -H "Content-Type: application/json" \
     -d "{\"role_id\": \"$VULTR_ROLE_ID\", \"session_name\": \"debug\", \"auth_method\": \"oidc\", \"duration\": 900}"'
```

**Solutions:**

1. **Token not mounted:**
   - Verify pod has the volume and volume mount
   - Check webhook logs for mutation
   - Delete and recreate the pod

2. **401 "Invalid API token":** this cluster's OIDC issuer is not registered
   with Vultr, or is registered under a different cluster ID. See
   "Registering your cluster" in the README.

3. **403 "No trust relationship exists":** the issuer is registered, but the
   role has no trust for it. Create one:
   ```bash
   curl -X POST https://api.vultr.com/v2/role-trusts \
     -H "Authorization: Bearer ${VULTR_API_KEY}" \
     -H "Content-Type: application/json" \
     -d '{
       "role_id": "ROLE-ID",
       "trust_type": "TemporaryAssumption",
       "trusted_oidc_issuer_id": "ISSUER-ID"
     }'
   ```
   Note that role trusts do not currently support a subject condition: a
   trust to an issuer applies to every ServiceAccount in that cluster.

4. **Wrong audience in token:**
   - Verify the projected token has audience "vultr"
   - `kubectl exec <pod-name> -- cat /var/run/secrets/vultr.com/serviceaccount/token | cut -d. -f2 | base64 -d`

5. **AWS SDK / STS-compatible endpoint:** the `AWS_ROLE_ARN` /
   `AWS_ENDPOINT_URL_STS` variables are still injected for existing
   consumers, but Vultr's STS-compatible endpoint does not currently issue
   credentials for any role. New consumers should use the native exchange
   above; this is a known server-side limitation, not a pod misconfiguration.

### 6. Performance Issues

**Symptoms:**
- Pod creation is slow
- Webhook timeout warnings
- High resource usage

**Diagnosis:**
```bash
# Check webhook resource usage
kubectl top pods -n irsa-system

# View webhook latency in logs
kubectl logs -n irsa-system -l app=irsa-webhook | grep "Processing pod"

# Check for throttling
kubectl describe pods -n irsa-system -l app=irsa-webhook
```

**Solutions:**

1. **Increase webhook timeout:**
   ```bash
   kubectl patch mutatingwebhookconfiguration irsa-webhook \
     --type='json' \
     -p='[{"op": "replace", "path": "/webhooks/0/timeoutSeconds", "value": 30}]'
   ```

2. **Scale webhook deployment:**
   ```bash
   kubectl scale deployment -n irsa-system irsa-webhook --replicas=3
   ```

3. **Increase resource limits:**
   Edit deploy/webhook.yaml and increase CPU/memory limits:
   ```yaml
   resources:
     requests:
       cpu: 200m
       memory: 256Mi
     limits:
       cpu: 1000m
       memory: 512Mi
   ```

### 7. JSON Patch Generation Errors

**Symptoms:**
- "Failed to generate patches" in webhook logs
- Malformed patch errors
- Array index out of bounds

**Diagnosis:**
```bash
# Check specific pod that failed
kubectl logs -n irsa-system -l app=irsa-webhook --tail=100 | grep -A 10 "Failed"
```

**Solutions:**

1. **Review pod specification:**
   - Ensure pod spec is valid JSON
   - Check for unusual container configurations

2. **Report it:** patch generation is covered by unit tests in
   `internal/mutate`, so a failure here usually means a pod shape those
   tests don't cover yet. Open an issue with the pod spec that triggered it.

### 8. Multiple Webhooks Conflict

**Symptoms:**
- Pod mutations from other webhooks interfering
- Unexpected pod configuration
- Volume/env var conflicts

**Diagnosis:**
```bash
# List all mutating webhooks
kubectl get mutatingwebhookconfigurations

# Check webhook order
kubectl get mutatingwebhookconfigurations -o yaml | grep -A 5 "name:"
```

**Solutions:**

1. **Adjust webhook order:**
   Webhooks are processed alphabetically by name. Rename if needed:
   ```bash
   # Add a prefix to control order
   kubectl patch mutatingwebhookconfiguration irsa-webhook \
     --type='json' \
     -p='[{"op": "replace", "path": "/metadata/name", "value": "01-irsa-webhook"}]'
   ```

2. **`reinvocationPolicy: IfNeeded` is already set** in `deploy/webhook.yaml`,
   so the API server will call this webhook again if a later webhook (for
   example a service-mesh sidecar injector) adds containers. Injection is
   idempotent, so repeated calls do not duplicate the volume or env vars.
   If you still see conflicts, check whether the other webhook is
   overwriting fields this one set, not the reverse.

## Debug Commands Cheat Sheet

```bash
# View all webhook-related resources
kubectl get all -n irsa-system
kubectl get mutatingwebhookconfiguration irsa-webhook
kubectl get clusterrole irsa-webhook
kubectl get clusterrolebinding irsa-webhook

# Test webhook directly
kubectl run test-pod --image=nginx --dry-run=client -o yaml | \
  kubectl create -f - --namespace=default

# Watch webhook logs in real-time
kubectl logs -n irsa-system -l app=irsa-webhook -f

# Check webhook pod health
kubectl get pods -n irsa-system -l app=irsa-webhook -o wide
kubectl describe pods -n irsa-system -l app=irsa-webhook

# View recent events
kubectl get events -n irsa-system --sort-by='.lastTimestamp'

# Test ServiceAccount annotation
kubectl get sa -A -o jsonpath='{range .items[?(@.metadata.annotations.api\.vultr\.com/role)]}{.metadata.namespace}{" "}{.metadata.name}{" "}{.metadata.annotations.api\.vultr\.com/role}{"\n"}{end}'

# Validate webhook configuration
kubectl get mutatingwebhookconfiguration irsa-webhook -o yaml | grep -E "(caBundle|service|path|port)"
```

## Getting Help

If you're still experiencing issues:

1. **Collect diagnostic information:**
   ```bash
   # Run this script and save output
   kubectl get all -n irsa-system > diagnostics.txt
   kubectl logs -n irsa-system -l app=irsa-webhook --tail=200 >> diagnostics.txt
   kubectl get mutatingwebhookconfiguration irsa-webhook -o yaml >> diagnostics.txt
   kubectl get events -n irsa-system >> diagnostics.txt
   ```

2. **Check webhook version:**
   ```bash
   kubectl get deployment -n irsa-system irsa-webhook -o jsonpath='{.spec.template.spec.containers[0].image}'
   ```

3. **Review logs with timestamps:**
   ```bash
   kubectl logs -n irsa-system -l app=irsa-webhook --timestamps=true --tail=100
   ```

4. **Test in isolation:**
   - Create a separate test namespace
   - Deploy a simple test pod
   - Monitor webhook behavior

## Prevention

**Best Practices to Avoid Issues:**

1. Always test in a non-production cluster first
2. Prefer the MutatingAdmissionPolicy mode on Kubernetes 1.36+; it has no certificates or availability concerns
3. Monitor webhook performance and logs
4. Webhook mode: rerun `generate-certs.sh` before the certificate expires (10 years by default)
5. Use resource limits to prevent webhook from consuming too much
6. Implement readiness and liveness probes
7. Scale webhook deployment for high-traffic clusters
8. Document all ServiceAccount annotations
