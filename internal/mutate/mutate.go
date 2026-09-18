// Package mutate builds the JSON patches that give a Pod access to a Vultr
// IAM role through a projected ServiceAccount token.
//
// Pods receive the projected token plus VULTR_ROLE_ID and
// VULTR_WEB_IDENTITY_TOKEN_FILE, which applications exchange at the native
// assume-role endpoint. The AWS_* variables are kept for consumers written
// against the STS-compatible endpoint.
//
// The output is deliberately idempotent: running the patches through the
// generator a second time (or against a Pod another webhook already injected
// into) produces no additional operations. That property is what makes it
// safe to register the webhook with reinvocationPolicy: IfNeeded.
package mutate

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

const (
	// RoleAnnotation is the ServiceAccount annotation that enables injection.
	RoleAnnotation = "api.vultr.com/role"

	// TokenVolumeName is the name of the projected token volume added to Pods.
	TokenVolumeName = "vultr-irsa-token"
	// TokenMountPath is where the projected token volume is mounted.
	TokenMountPath = "/var/run/secrets/vultr.com/serviceaccount"
	// TokenFileName is the file name of the token inside the mount.
	TokenFileName = "token"
	// TokenAudience is the audience requested for the projected token.
	TokenAudience = "vultr"

	// DefaultSTSEndpoint is the AWS-compatible STS endpoint used when none is configured.
	DefaultSTSEndpoint = "https://api.vultr.com/v2/assumed-roles/compatibility/aws/sts"
	// DefaultTokenExpirationSeconds is the projected token lifetime when none is configured.
	DefaultTokenExpirationSeconds int64 = 86400

	envVultrRoleID            = "VULTR_ROLE_ID"
	envVultrTokenFile         = "VULTR_WEB_IDENTITY_TOKEN_FILE"
	envAWSRoleArn             = "AWS_ROLE_ARN"
	envAWSWebIdentityToken    = "AWS_WEB_IDENTITY_TOKEN_FILE"
	envAWSSTSRegionalEndpoint = "AWS_STS_REGIONAL_ENDPOINTS"
	envAWSEndpointURLSTS      = "AWS_ENDPOINT_URL_STS"
)

// JSONPatch is a single RFC 6902 operation.
type JSONPatch struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Config holds the values injected into every mutated Pod.
type Config struct {
	// STSEndpoint is the value of AWS_ENDPOINT_URL_STS.
	STSEndpoint string
	// TokenExpirationSeconds is the projected token lifetime.
	TokenExpirationSeconds int64
}

// DefaultConfig returns a Config with the production defaults.
func DefaultConfig() Config {
	return Config{
		STSEndpoint:            DefaultSTSEndpoint,
		TokenExpirationSeconds: DefaultTokenExpirationSeconds,
	}
}

// Mutator generates patches for a given Config.
type Mutator struct {
	cfg Config
}

// New returns a Mutator. Zero-valued Config fields fall back to defaults.
func New(cfg Config) *Mutator {
	def := DefaultConfig()
	if cfg.STSEndpoint == "" {
		cfg.STSEndpoint = def.STSEndpoint
	}
	if cfg.TokenExpirationSeconds <= 0 {
		cfg.TokenExpirationSeconds = def.TokenExpirationSeconds
	}
	return &Mutator{cfg: cfg}
}

// Patches returns the JSON patch operations that inject credentials for role
// into pod. It returns an empty slice when the Pod is already fully injected.
func (m *Mutator) Patches(pod *corev1.Pod, role string) []JSONPatch {
	var patches []JSONPatch

	if !hasVolume(pod.Spec.Volumes, TokenVolumeName) {
		vol := m.tokenVolume()
		if pod.Spec.Volumes == nil {
			patches = append(patches, JSONPatch{Op: "add", Path: "/spec/volumes", Value: []corev1.Volume{vol}})
		} else {
			patches = append(patches, JSONPatch{Op: "add", Path: "/spec/volumes/-", Value: vol})
		}
	}

	for i := range pod.Spec.Containers {
		patches = append(patches, m.containerPatches("/spec/containers", i, &pod.Spec.Containers[i], role)...)
	}
	for i := range pod.Spec.InitContainers {
		patches = append(patches, m.containerPatches("/spec/initContainers", i, &pod.Spec.InitContainers[i], role)...)
	}

	return patches
}

func (m *Mutator) tokenVolume() corev1.Volume {
	expiry := m.cfg.TokenExpirationSeconds
	return corev1.Volume{
		Name: TokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Audience:          TokenAudience,
						ExpirationSeconds: &expiry,
						Path:              TokenFileName,
					},
				}},
			},
		},
	}
}

func (m *Mutator) envVars(role string) []corev1.EnvVar {
	tokenPath := TokenMountPath + "/" + TokenFileName
	return []corev1.EnvVar{
		{Name: envVultrRoleID, Value: role},
		{Name: envVultrTokenFile, Value: tokenPath},
		{Name: envAWSRoleArn, Value: role},
		{Name: envAWSWebIdentityToken, Value: tokenPath},
		{Name: envAWSSTSRegionalEndpoint, Value: "regional"},
		{Name: envAWSEndpointURLSTS, Value: m.cfg.STSEndpoint},
	}
}

func (m *Mutator) containerPatches(basePath string, index int, c *corev1.Container, role string) []JSONPatch {
	var patches []JSONPatch
	base := fmt.Sprintf("%s/%d", basePath, index)

	if !hasMount(c.VolumeMounts, TokenVolumeName) {
		mount := corev1.VolumeMount{Name: TokenVolumeName, MountPath: TokenMountPath, ReadOnly: true}
		if c.VolumeMounts == nil {
			patches = append(patches, JSONPatch{Op: "add", Path: base + "/volumeMounts", Value: []corev1.VolumeMount{mount}})
		} else {
			patches = append(patches, JSONPatch{Op: "add", Path: base + "/volumeMounts/-", Value: mount})
		}
	}

	// Variables the operator (or an earlier invocation) already set are
	// left untouched; only the missing ones are added.
	missing := make([]corev1.EnvVar, 0, 6)
	for _, e := range m.envVars(role) {
		if !hasEnv(c.Env, e.Name) {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		if c.Env == nil {
			patches = append(patches, JSONPatch{Op: "add", Path: base + "/env", Value: missing})
		} else {
			for _, e := range missing {
				patches = append(patches, JSONPatch{Op: "add", Path: base + "/env/-", Value: e})
			}
		}
	}
	return patches
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func hasMount(mounts []corev1.VolumeMount, name string) bool {
	for _, mnt := range mounts {
		if mnt.Name == name {
			return true
		}
	}
	return false
}

func hasEnv(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}
