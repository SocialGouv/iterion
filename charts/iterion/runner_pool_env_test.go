package chart_test

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestChartRenderRunnerPoolCarriesNoPlatformKeyMaterial(t *testing.T) {
	for _, existingAuth := range []bool{false, true} {
		t.Run(fmt.Sprintf("external-auth=%t", existingAuth), func(t *testing.T) {
			auth := "iterion-auth"
			keyMaterial := []string{"fixture-ring-simple-Q72m8", "fixture-ring-current-A43z9", "fixture-ring-prior-B61x2"}
			authFixture := valuesFile(t, fmt.Sprintf(`secrets:
  auth:
    secretsKey: %s
    secretsKeys: "v1=%s,v0=%s"
    secretsKeyID: v1
`, keyMaterial[0], keyMaterial[1], keyMaterial[2]))
			args := []string{"-f", "values-prod.yaml", "-f", "testdata/values-pools.yaml", "-f", authFixture,
				"--set", "secrets.storage.existingSecret=pool-storage",
				"--set", "runnerPools[0].gateway.apiKeySecretKey=custom-gateway-key"}
			if existingAuth {
				auth = "external-auth"
				args = append(args, "--set", "secrets.auth.existingSecret="+auth)
			} else {
				args = append(args, "--set", "secrets.auth.existingSecret=", "--set", "secrets.auth.create=true",
					"--set", "secrets.auth.jwtSecret=fixture-jwt")
			}
			docs := manifests(t, mustRender(t, args...))
			for _, pool := range []string{"alpha", "solo"} {
				spec := podSpec(t, docs, "Deployment", "iterion-runner-pool-"+pool)
				gatewayKey := "OPENAI_COMPATIBLE_API_KEY"
				if pool == "alpha" {
					gatewayKey = "custom-gateway-key"
				}
				template, ok := dig(must(t, docs, "Deployment", "iterion-runner-pool-"+pool), "spec", "template").(map[string]any)
				if !ok {
					t.Fatal("missing PodTemplate")
				}
				assertPoolKeyMaterialAbsent(t, docs, template, keyMaterial, map[string]map[string]bool{
					auth:                                {"ITERION_JWT_SECRET": true, "ITERION_MONGO_URI": true},
					"iterion-pool-" + pool + "-gateway": {gatewayKey: true},
				})
				for _, group := range []string{"containers", "initContainers"} {
					containers, _ := spec[group].([]any)
					for _, container := range containers {
						refs := map[string]string{}
						env, _ := dig(container, "env").([]any)
						for _, e := range env {
							name, _ := dig(e, "name").(string)
							if strings.HasPrefix(name, "ITERION_SECRETS_KEY") {
								t.Fatalf("%s exposes platform key env %s", pool, name)
							}
							ref, _ := dig(e, "valueFrom", "secretKeyRef").(map[string]any)
							if ref == nil {
								continue
							}
							if key, _ := ref["key"].(string); strings.HasPrefix(key, "ITERION_SECRETS_KEY") {
								t.Fatalf("%s aliases platform key via %s", pool, name)
							}
							refs[name] = fmt.Sprint(ref["name"]) + "/" + fmt.Sprint(ref["key"])
						}
						if dig(container, "name") == "iterion-runner" {
							gatewayKey := "OPENAI_COMPATIBLE_API_KEY"
							if pool == "alpha" {
								gatewayKey = "custom-gateway-key"
							}
							want := map[string]string{
								"ITERION_JWT_SECRET":        auth + "/ITERION_JWT_SECRET",
								"ITERION_MONGO_URI":         auth + "/ITERION_MONGO_URI",
								"OPENAI_COMPATIBLE_API_KEY": "iterion-pool-" + pool + "-gateway/" + gatewayKey,
							}
							if !reflect.DeepEqual(refs, want) {
								t.Fatalf("%s key refs = %v, want %v", pool, refs, want)
							}
						}
						from, _ := dig(container, "envFrom").([]any)
						for _, entry := range from {
							if secret := dig(entry, "secretRef", "name"); secret != nil && secret != "pool-storage" {
								t.Fatalf("%s imports whole secret %v", pool, secret)
							}
							if config := dig(entry, "configMapRef", "name"); config != nil {
								data, _ := dig(must(t, docs, "ConfigMap", fmt.Sprint(config)), "data").(map[string]any)
								for key := range data {
									if strings.HasPrefix(key, "ITERION_SECRETS_KEY") {
										t.Fatalf("%s imports platform key %s via configmap", pool, key)
									}
								}
							}
						}
					}
				}
			}
		})
	}
}

func TestChartRenderRunnerPoolPinsItsEnvBlock(t *testing.T) {
	values := valuesFile(t, `runnerPools:
  - name: alpha
    gateway:
      baseUrl: https://gateway.alpha.example
      existingSecret: iterion-pool-alpha-gateway
    env:
      ITERION_MODEL_AUDIT: local-model
      ITERION_MAX_COST_USD: "0"
      EXTERNAL_SETTING: value
      ITERION_BOT_VARS_ENV_PINS: " ITERION_PREEXISTING,ITERION_MODEL_AUDIT,, ITERION_PREEXISTING "
`)
	docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "-f", values))
	spec := podSpec(t, docs, "Deployment", "iterion-runner-pool-alpha")
	env, _ := dig(spec, "containers", "0", "env").([]any)
	found := map[string]string{}
	counts := map[string]int{}
	for _, e := range env {
		name, _ := dig(e, "name").(string)
		if value, ok := dig(e, "value").(string); ok {
			found[name] = value
		}
		counts[name]++
	}
	want := "ITERION_MAX_COST_USD,ITERION_MODEL_AUDIT,ITERION_PREEXISTING"
	if found["ITERION_BOT_VARS_ENV_PINS"] != want || counts["ITERION_BOT_VARS_ENV_PINS"] != 1 {
		t.Fatalf("pins = %q (%d occurrences), want %q once", found["ITERION_BOT_VARS_ENV_PINS"], counts["ITERION_BOT_VARS_ENV_PINS"], want)
	}
	for name, value := range map[string]string{"ITERION_MODEL_AUDIT": "local-model", "ITERION_MAX_COST_USD": "0", "EXTERNAL_SETTING": "value"} {
		if found[name] != value || counts[name] != 1 {
			t.Fatalf("%s = %q (%d occurrences), want %q once", name, found[name], counts[name], value)
		}
	}
}

// Inspect only data a pod can actually reach. Reading one JWT/Mongo key does
// not mount the platform ring beside it in the auth Secret. The approved
// operator storage Secret is opaque; a render cannot attest its contents.
func assertPoolKeyMaterialAbsent(t *testing.T, docs []map[string]any, pod map[string]any, canaries []string, allowed map[string]map[string]bool) {
	t.Helper()
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}
	var inspect func(string, any)
	inspect = func(path string, value any) {
		switch v := value.(type) {
		case string:
			candidates := []string{v}
			for _, enc := range encodings {
				if b, err := enc.DecodeString(v); err == nil {
					candidates = append(candidates, string(b))
				}
			}
			for _, key := range canaries {
				for _, candidate := range candidates {
					if strings.Contains(candidate, key) {
						t.Fatalf("platform key material reaches %s", path)
					}
				}
				for _, enc := range encodings {
					if strings.Contains(v, enc.EncodeToString([]byte(key))) {
						t.Fatalf("encoded platform key material reaches %s", path)
					}
				}
			}
		case map[string]any:
			for k, child := range v {
				inspect(path+".<key>", k)
				inspect(path+"."+k, child)
			}
		case []any:
			for i, child := range v {
				inspect(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	inspect("pod", pod)
	readRef := func(kind, name, key, path string) {
		if name == "" {
			t.Fatalf("empty %s reference at %s", kind, path)
		}
		if strings.HasPrefix(key, "ITERION_SECRETS_KEY") {
			t.Fatalf("platform key reference at %s", path)
		}
		if kind == "Secret" && name != "pool-storage" {
			if key == "" || !allowed[name][key] {
				t.Fatalf("unapproved Secret reference %s/%s at %s", name, key, path)
			}
		}
		doc := find(docs, kind, name)
		if doc == nil {
			if kind == "ConfigMap" {
				t.Fatalf("unverified ConfigMap %s at %s", name, path)
			}
			return // Only the explicitly approved external Secret refs reach here.
		}
		for _, field := range []string{"data", "stringData", "binaryData"} {
			data, _ := doc[field].(map[string]any)
			if key != "" {
				inspect(path+"."+field+"."+key, data[key])
				continue
			}
			for k, value := range data {
				inspect(path+"."+field+".<key>", k)
				if strings.HasPrefix(k, "ITERION_SECRETS_KEY") {
					t.Fatalf("platform key %s imported at %s", k, path)
				}
				inspect(path+"."+field+"."+k, value)
			}
		}
	}
	var walk func(string, any)
	walk = func(path string, value any) {
		switch v := value.(type) {
		case map[string]any:
			for _, ref := range []struct{ field, kind string }{{"secretKeyRef", "Secret"}, {"configMapKeyRef", "ConfigMap"}, {"secretRef", "Secret"}, {"configMapRef", "ConfigMap"}} {
				if m, ok := v[ref.field].(map[string]any); ok {
					name, _ := m["name"].(string)
					key, _ := m["key"].(string)
					readRef(ref.kind, name, key, path+"."+ref.field)
				}
			}
			// Both volumes and projected volume sources select either all keys
			// or explicit items. Container mount aliases do not hide the data.
			for _, ref := range []struct{ field, kind string }{{"secret", "Secret"}, {"configMap", "ConfigMap"}} {
				if m, ok := v[ref.field].(map[string]any); ok {
					name, _ := m["name"].(string)
					if n, ok := m["secretName"].(string); ok {
						name = n
					}
					items, _ := m["items"].([]any)
					if len(items) == 0 {
						readRef(ref.kind, name, "", path+"."+ref.field)
					}
					for _, item := range items {
						key, _ := dig(item, "key").(string)
						readRef(ref.kind, name, key, path+"."+ref.field)
					}
				}
			}
			for k, child := range v {
				walk(path+"."+k, child)
			}
		case []any:
			for i, child := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("pod", pod)
}
