## Unreleased
### Enhancements
- Inject `VULTR_ROLE_ID` and `VULTR_WEB_IDENTITY_TOKEN_FILE` so applications exchange the projected token at the native `/v2/assumed-roles/assume` endpoint; this is the primary path and the README now documents it first.
- Add `deploy/mutating-admission-policy.yaml`: IRSA injection as a MutatingAdmissionPolicy with no webhook, Deployment or TLS certificates (Kubernetes 1.36+)
- Make webhook mutation idempotent and enable `reinvocationPolicy: IfNeeded`
- Fail closed by default; exclude system namespaces and `irsa-system`; add PodDisruptionBudget and anti-affinity
- Add `TOKEN_EXPIRATION_SECONDS` to configure the projected token lifetime
- Add unit tests for patch generation and the admission handler
- Add Apache 2.0 license; fix module path to `github.com/vultr/irsa-webhook`
### Breaking
- `deploy.yaml` moved to `deploy/webhook.yaml`
- `failurePolicy` default changed from `Ignore` to `Fail`

## [v0.3.0](https://github.com/vultr/irsa-webhook/compare/v0.2.0..v0.3.0) (2026-05-04)
### Enhancements
- Add bind env variable [PR 17](https://github.com/vultr/irsa-webhook/pull/17)

## [v0.2.0](https://github.com/vultr/irsa-webhook/compare/v0.1.0..v0.2.0) (2026-04-30)
### Enhancements
- Add kubeconfig env support [PR 15](https://github.com/vultr/irsa-webhook/pull/15)

## v0.1.0 (2026-04-24)
* Initial release of IRSA webhook for Vultr
