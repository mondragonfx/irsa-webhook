package mutate

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const testRole = "775a6be6-45cd-4f19-94f5-6e4f96f093ec"

func ops(patches []JSONPatch) []string {
	out := make([]string, 0, len(patches))
	for _, p := range patches {
		out = append(out, p.Op+" "+p.Path)
	}
	return out
}

func assertOps(t *testing.T, got []JSONPatch, want ...string) {
	t.Helper()
	g := ops(got)
	if len(g) != len(want) {
		t.Fatalf("got %d patches %v, want %d %v", len(g), g, len(want), want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("patch %d: got %q, want %q (all: %v)", i, g[i], want[i], g)
		}
	}
}

func TestBarePod(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	got := New(Config{}).Patches(pod, testRole)
	assertOps(t, got,
		"add /spec/volumes",
		"add /spec/containers/0/volumeMounts",
		"add /spec/containers/0/env",
	)

	vols := got[0].Value.([]corev1.Volume)
	sat := vols[0].Projected.Sources[0].ServiceAccountToken
	if vols[0].Name != TokenVolumeName || sat.Audience != TokenAudience || sat.Path != TokenFileName || *sat.ExpirationSeconds != DefaultTokenExpirationSeconds {
		t.Fatalf("unexpected token volume %#v", vols[0])
	}

	env := got[2].Value.([]corev1.EnvVar)
	want := []string{envVultrRoleID, envVultrTokenFile, envAWSRoleArn, envAWSWebIdentityToken, envAWSSTSRegionalEndpoint, envAWSEndpointURLSTS}
	if len(env) != len(want) {
		t.Fatalf("env %v, want %v", env, want)
	}
	for i, e := range env {
		if e.Name != want[i] {
			t.Fatalf("env[%d] = %s, want %s", i, e.Name, want[i])
		}
		switch e.Name {
		case envVultrRoleID, envAWSRoleArn:
			if e.Value != testRole {
				t.Fatalf("%s = %q", e.Name, e.Value)
			}
		case envVultrTokenFile, envAWSWebIdentityToken:
			if e.Value != TokenMountPath+"/"+TokenFileName {
				t.Fatalf("%s = %q", e.Name, e.Value)
			}
		case envAWSEndpointURLSTS:
			if e.Value != DefaultSTSEndpoint {
				t.Fatalf("%s = %q", e.Name, e.Value)
			}
		}
	}
}

func TestExistingFieldsAreAppended(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{{Name: "scratch"}},
		InitContainers: []corev1.Container{{
			Name: "init",
			Env:  []corev1.EnvVar{{Name: "EXISTING", Value: "keep"}},
		}},
		Containers: []corev1.Container{{
			Name:         "app",
			Env:          []corev1.EnvVar{{Name: "EXISTING", Value: "keep"}},
			VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
		}},
	}}
	got := New(Config{}).Patches(pod, testRole)
	assertOps(t, got,
		"add /spec/volumes/-",
		"add /spec/containers/0/volumeMounts/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/initContainers/0/volumeMounts",
		"add /spec/initContainers/0/env/-",
		"add /spec/initContainers/0/env/-",
		"add /spec/initContainers/0/env/-",
		"add /spec/initContainers/0/env/-",
		"add /spec/initContainers/0/env/-",
		"add /spec/initContainers/0/env/-",
	)
}

func injectedPod(m *Mutator) *corev1.Pod {
	expiry := DefaultTokenExpirationSeconds
	c := corev1.Container{
		Name:         "app",
		Env:          append([]corev1.EnvVar{{Name: "EXISTING"}}, m.envVars(testRole)...),
		VolumeMounts: []corev1.VolumeMount{{Name: TokenVolumeName, MountPath: TokenMountPath, ReadOnly: true}},
	}
	return &corev1.Pod{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{{
			Name: TokenVolumeName,
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience: TokenAudience, ExpirationSeconds: &expiry, Path: TokenFileName,
				}}},
			}},
		}},
		Containers:     []corev1.Container{c},
		InitContainers: []corev1.Container{c},
	}}
}

func TestIdempotent(t *testing.T) {
	m := New(Config{})
	if got := m.Patches(injectedPod(m), testRole); len(got) != 0 {
		t.Fatalf("expected no patches for an already-injected pod, got %v", ops(got))
	}
}

func TestReinvocationInjectsOnlyNewContainers(t *testing.T) {
	// Simulates a second invocation after another webhook added a sidecar:
	// the first container was already injected, the sidecar was not.
	m := New(Config{})
	pod := injectedPod(m)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar"})
	got := m.Patches(pod, testRole)
	assertOps(t, got,
		"add /spec/containers/1/volumeMounts",
		"add /spec/containers/1/env",
	)
}

func TestPartialEnvIsCompleted(t *testing.T) {
	// A container that set AWS_STS_REGIONAL_ENDPOINTS itself should receive
	// the remaining variables and keep its own value.
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app",
		Env:  []corev1.EnvVar{{Name: envAWSSTSRegionalEndpoint, Value: "legacy"}},
	}}}}
	got := New(Config{}).Patches(pod, testRole)
	assertOps(t, got,
		"add /spec/volumes",
		"add /spec/containers/0/volumeMounts",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
		"add /spec/containers/0/env/-",
	)
	for _, p := range got[2:] {
		if e := p.Value.(corev1.EnvVar); e.Name == envAWSSTSRegionalEndpoint {
			t.Fatalf("operator-set %s must not be overridden", envAWSSTSRegionalEndpoint)
		}
	}
}

func TestConfigOverrides(t *testing.T) {
	m := New(Config{STSEndpoint: "https://sts.example", TokenExpirationSeconds: 3600})
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	got := m.Patches(pod, testRole)
	vols := got[0].Value.([]corev1.Volume)
	if *vols[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds != 3600 {
		t.Fatalf("expiration override not applied")
	}
	for _, e := range got[2].Value.([]corev1.EnvVar) {
		if e.Name == envAWSEndpointURLSTS && e.Value != "https://sts.example" {
			t.Fatalf("STS endpoint override not applied: %q", e.Value)
		}
	}
}

func TestPatchesMarshalToJSONPatch(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	raw, err := json.Marshal(New(Config{}).Patches(pod, testRole))
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, op := range decoded {
		if op["op"] != "add" || op["path"] == "" || op["value"] == nil {
			t.Fatalf("malformed patch op %v", op)
		}
	}
}
