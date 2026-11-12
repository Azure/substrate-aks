package installer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFingerprintDeploymentContract(t *testing.T) {
	const expected = "10c1894326773f77630e1daa58cf85dbde0cd5764b4c86324ffc3657d2db2463"
	if got := fingerprint(testConfig(t), testInfrastructure()); got != expected {
		t.Fatalf("deployment contract changed the installation fingerprint: %s", got)
	}
}

func TestFingerprintSizingIdentity(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	base := fingerprint(c, i)
	c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{Resources: &ResourceRequirements{
		Requests: map[string]string{"cpu": "1000m"},
	}}}
	c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("60s")}
	got := fingerprint(c, i)
	if got == base || got != fingerprint(c, i) {
		t.Fatal("configured sizing did not get a deterministic distinct identity")
	}
	c.Substrate.Sizing.APIServer.Resources.Requests["cpu"] = "1"
	c.Substrate.Atelet.SnapshotUploadDelay = stringPointer("1m")
	if got != fingerprint(c, i) {
		t.Fatal("equivalent quantities or durations changed identity")
	}
	c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{Resources: &ResourceRequirements{}}}
	c.Substrate.Atelet = &AteletConfig{}
	if fingerprint(c, i) != base {
		t.Fatal("empty settings changed the unset identity")
	}
}

func TestFingerprintEachSizingField(t *testing.T) {
	replicas, pool := 3, 10
	for _, tc := range []struct {
		name   string
		change func(*SubstrateConfig)
	}{
		{"api replicas", func(s *SubstrateConfig) {
			s.Sizing = &SizingConfig{APIServer: &APIServerSizing{Replicas: &replicas, PostgresPoolMaxConns: &pool}}
		}},
		{"pool", func(s *SubstrateConfig) {
			s.Sizing = &SizingConfig{APIServer: &APIServerSizing{PostgresPoolMaxConns: &pool}}
		}},
		{"api requests", func(s *SubstrateConfig) {
			s.Sizing = &SizingConfig{APIServer: &APIServerSizing{Resources: &ResourceRequirements{Requests: map[string]string{"cpu": "500m"}}}}
		}},
		{"api limits", func(s *SubstrateConfig) {
			s.Sizing = &SizingConfig{APIServer: &APIServerSizing{Resources: &ResourceRequirements{Limits: map[string]string{"memory": "1Gi"}}}}
		}},
		{"router replicas", func(s *SubstrateConfig) { s.Sizing = &SizingConfig{Router: &RouterSizing{Replicas: &replicas}} }},
		{"router resources", func(s *SubstrateConfig) {
			s.Sizing = &SizingConfig{Router: &RouterSizing{Resources: &RouterResources{AtenetRouter: &ResourceRequirements{Requests: map[string]string{"cpu": "1"}}}}}
		}},
		{"envoy resources", func(s *SubstrateConfig) {
			s.Sizing = &SizingConfig{Router: &RouterSizing{Resources: &RouterResources{Envoy: &ResourceRequirements{Requests: map[string]string{"cpu": "1"}}}}}
		}},
		{"upload delay", func(s *SubstrateConfig) { s.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("1m")} }},
		{"detach delay", func(s *SubstrateConfig) { s.Atelet = &AteletConfig{SnapshotDetachDelay: stringPointer("6m")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, i := testConfig(t), testInfrastructure()
			base := fingerprint(c, i)
			tc.change(&c.Substrate)
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if fp := fingerprint(c, i); fp == base || fp != fingerprint(c, i) {
				t.Fatal("configured setting did not produce a deterministic distinct identity")
			}
		})
	}
}

func TestPreparedSourceRequiresExactFilesAndRouterHash(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string, *prepared)
	}{
		{"missing marker path", func(_ *testing.T, _ string, p *prepared) { delete(p.Files, adaptedSourcePaths[6]) }},
		{"substituted marker path", func(_ *testing.T, _ string, p *prepared) {
			delete(p.Files, adaptedSourcePaths[6])
			p.Files["other.yaml"] = checksum(nil)
		}},
		{"missing router", func(t *testing.T, root string, _ *prepared) {
			if err := os.Remove(filepath.Join(root, adaptedSourcePaths[6])); err != nil {
				t.Fatal(err)
			}
		}},
		{"modified router", func(t *testing.T, root string, _ *prepared) {
			if err := os.WriteFile(filepath.Join(root, adaptedSourcePaths[6]), []byte("modified"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, i := testConfig(t), testInfrastructure()
			root, _ := clientFixture(t, c)
			marker := filepath.Join(root, ".substrate-aks-prepared.json")
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			var p prepared
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			tc.change(t, root, &p)
			data, err = json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := (App{Config: c, Runner: script(t)}).prepare(t.Context(), i); err == nil {
				t.Fatal("accepted invalid managed source")
			}
		})
	}
}

func TestPreparedSourceRejectsModifiedFiles(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	fp := fingerprint(c, i)
	root := filepath.Join(c.workspace(), "source-"+fp+"-v3")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := prepared{Fingerprint: fp, Files: map[string]string{}}
	for _, name := range adaptedSourcePaths {
		data := []byte("prepared " + name)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		p.Files[name] = checksum(data)
	}
	marker, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".substrate-aks-prepared.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("unexpected Go modification", func(t *testing.T) {
		r := script(t, reply{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
			reply{contains: "git diff --name-only HEAD", output: "cmd/ate-setup/main.go\n"})
		_, err := (App{Config: c, Runner: r}).prepare(t.Context(), i)
		if err == nil || !strings.Contains(err.Error(), "unexpected tracked source modification") {
			t.Fatalf("accepted modified source: %v", err)
		}
	})
	t.Run("untracked enumeration failure", func(t *testing.T) {
		want := errors.New("cannot enumerate source files")
		r := script(t, reply{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
			reply{contains: "git diff --name-only HEAD"},
			reply{contains: "git ls-files --others -z --", err: want})
		if _, err := (App{Config: c, Runner: r}).prepare(t.Context(), i); !errors.Is(err, want) {
			t.Fatalf("lost source verification failure: %v", err)
		}
	})
	t.Run("modified adapter output", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, adaptedSourcePaths[0]), []byte("reverted to GCS"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := (App{Config: c, Runner: script(t)}).prepare(t.Context(), i)
		if err == nil || !strings.Contains(err.Error(), "managed source changed") {
			t.Fatalf("accepted modified adapter output: %v", err)
		}
	})
}

func TestPreparedSourceChecksUntrackedFiles(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	root, _ := clientFixture(t, c)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{}
	git := func(args ...string) []byte {
		t.Helper()
		data, err := runner.Run(t.Context(), Command{Dir: root, Args: append([]string{"git"}, args...)})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	git("init", "--quiet")
	git("add", "--", ".", ":!.substrate-aks-prepared.json")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath="+t.TempDir(),
		"commit", "--quiet", "-m", "Prepared-source test fixture")
	c.Substrate.Source.Commit = strings.TrimSpace(string(git("rev-parse", "HEAD")))
	marker, err := os.ReadFile(filepath.Join(root, ".substrate-aks-prepared.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p prepared
	if err := json.Unmarshal(marker, &p); err != nil {
		t.Fatal(err)
	}
	p.Fingerprint = fingerprint(c, i)
	dest := filepath.Join(c.workspace(), "source-"+p.Fingerprint+"-v3")
	if err := os.Rename(root, dest); err != nil {
		t.Fatal(err)
	}
	marker, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".substrate-aks-prepared.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	a := App{Config: c, Runner: runner}
	if got, err := a.prepare(t.Context(), i); err != nil || got != dest {
		t.Fatalf("rejected the installer's untracked completion marker: %q, %v", got, err)
	}
	for _, path := range []string{"cmd/ate-setup/extra.go", "cmd/kubectl-ate/extra.go", "ignored.go", ".substrate-aks-prepared.json.go"} {
		t.Run(path, func(t *testing.T) {
			file := filepath.Join(dest, path)
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("package main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(file); err != nil {
					t.Error(err)
				}
			})
			if _, err := a.prepare(t.Context(), i); err == nil ||
				!strings.Contains(err.Error(), "unexpected untracked source file") || !strings.Contains(err.Error(), path) {
				t.Fatalf("accepted added Go source %s: %v", path, err)
			}
		})
	}
}

func TestUpstreamCommandsPinTargetAndImages(t *testing.T) {
	c := testConfig(t)
	r := script(t, reply{contains: "go run ./cmd/ate-setup deploy atelet"})
	if err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", "deploy", "atelet"); err != nil {
		t.Fatal(err)
	}

	call := r.calls[0]
	command := strings.Join(call.Args, " ")
	for _, want := range []string{
		"--context " + c.AKS.ClusterName, "--kubeconfig " + filepath.Join(c.workspace(), "kubeconfig"),
		"--image-repo " + c.Substrate.Images.Repository, "--image-tag " + c.Substrate.Images.Tag,
		"--snapshot-location azblob://snapshots/cluster/", "--no-dev-env",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("upstream command lost %s: %v", want, call.Args)
		}
	}
	if strings.Contains(command, "--kind") || call.Dir != "/managed/source" {
		t.Fatalf("command used the wrong deployment profile or checkout: %+v", call)
	}
}

func TestUpstreamPassesConfiguredPool(t *testing.T) {
	c := testConfig(t)
	pool := 12
	c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{PostgresPoolMaxConns: &pool}}
	r := script(t, reply{contains: "go run ./cmd/ate-setup deploy ate-system"})
	if err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", "deploy", "ate-system"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.calls[0].Args, " "), "--ateapi-postgres-pool-max-conns 12") {
		t.Fatalf("pool flag missing: %v", r.calls[0].Args)
	}
}

func TestRepeatedInstallKeepsSizing(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	c.Substrate.Runtime.AssetsDir = t.TempDir()
	replicas, pool, routers := 3, 10, 2
	c.Substrate.Sizing = &SizingConfig{
		APIServer: &APIServerSizing{Replicas: &replicas, PostgresPoolMaxConns: &pool,
			Resources: &ResourceRequirements{Requests: map[string]string{"cpu": "500m"}, Limits: map[string]string{"memory": "1Gi"}}},
		Router: &RouterSizing{Replicas: &routers, Resources: &RouterResources{
			AtenetRouter: &ResourceRequirements{Requests: map[string]string{"cpu": "250m"}},
			Envoy:        &ResourceRequirements{Limits: map[string]string{"memory": "512Mi"}},
		}},
	}
	c.Substrate.Atelet = &AteletConfig{stringPointer("10s"), stringPointer("30s")}
	root, _ := clientFixture(t, c)
	writeAssetFixture(t, root, c.Substrate.Runtime.AssetsDir)
	api := []byte(strings.Replace(string(workloadFixture(false)), "- name: atelet\n        image:", "- name: ate-api-server\n        image:", 1))
	router := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: atenet-router
  namespace: ate-system
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: atenet-router
        image: example.invalid/router
      - name: envoy
        image: example.invalid/envoy
`)
	plugin := testPlugin(t, c, i)
	api, err := adaptWorkload(api, c, plugin, false)
	if err != nil {
		t.Fatal(err)
	}
	atelet, err := adaptWorkload(workloadFixture(true), c, plugin, true)
	if err != nil {
		t.Fatal(err)
	}
	router, err = adaptRouter(router, c)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ".substrate-aks-prepared.json")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var p prepared
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	for rel, content := range map[string][]byte{adaptedSourcePaths[0]: atelet, adaptedSourcePaths[1]: api, adaptedSourcePaths[6]: router} {
		if err := os.WriteFile(filepath.Join(root, rel), content, 0600); err != nil {
			t.Fatal(err)
		}
		p.Files[rel] = checksum(content)
	}
	data, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, data, 0600); err != nil {
		t.Fatal(err)
	}
	record := `{"data":{"installationId":"` + c.Installation.ID + `","fingerprint":"` + fingerprint(c, i) + `","phase":"deployed"}}`
	var replies []reply
	var renders [][]string
	for range 2 {
		replies = append(replies, installGuardReplies(t, c, record)...)
		replies = append(replies, imageCheckReplies(c, `{"status":"available","checked":18,"expected":18}`)...)
		replies = append(replies,
			reply{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
			reply{contains: "git diff --name-only HEAD"},
			reply{contains: "git ls-files --others -z --", output: ".substrate-aks-prepared.json\x00"},
			reply{contains: "get --raw /version", output: `{"gitVersion":"v1.37.1"}`},
			reply{contains: "api-resources", output: "podcertificaterequests.certificates.k8s.io\nclustertrustbundles.certificates.k8s.io"},
			reply{contains: "wait node"},
			reply{contains: "get nodes", output: `{"items":[{"metadata":{"labels":{"substrate-aks/runtime":"microvm"}},"status":{"nodeInfo":{"kubeletVersion":"v1.37.1","architecture":"amd64","operatingSystem":"linux"}}}]}`},
			reply{contains: "apply -f -"}, reply{contains: "rollout status daemonset/substrate-aks-kvm-probe"},
			reply{contains: "delete daemonset substrate-aks-kvm-probe"})
		for range 4 {
			replies = append(replies, reply{contains: "storage blob upload"})
		}
		replies = append(replies,
			reply{contains: "apply -f -"},
			reply{contains: "go run ./cmd/ate-setup deploy ate-system", effect: func(cmd Command) {
				var files []string
				for _, rel := range []string{adaptedSourcePaths[0], adaptedSourcePaths[1], adaptedSourcePaths[6]} {
					content, err := os.ReadFile(filepath.Join(cmd.Dir, rel))
					if err != nil {
						t.Fatal(err)
					}
					files = append(files, string(content))
				}
				if !strings.Contains(strings.Join(cmd.Args, " "), "--ateapi-postgres-pool-max-conns 10") {
					t.Fatal("install did not pass the configured pool")
				}
				renders = append(renders, files)
			}},
			reply{contains: "apply -f -"}, reply{contains: "apply -f -"})
	}
	r := imageBuildRunner{script(t, replies...), c.Substrate.Runtime.AssetsDir}
	for range 2 {
		if err := (App{Config: c, Runner: r}).Install(t.Context(), "ate-system"); err != nil {
			t.Fatal(err)
		}
	}
	if len(renders) != 2 || !reflect.DeepEqual(renders[0], renders[1]) ||
		!strings.Contains(renders[0][0], "--snapshot-upload-delay=10s") ||
		!strings.Contains(renders[0][0], "--snapshot-detach-delay=30s") ||
		!strings.Contains(renders[0][1], "cpu: 500m") ||
		!strings.Contains(renders[0][2], "replicas: 2") {
		t.Fatalf("repeated install lost sizing: %v", renders)
	}
	for _, workload := range []struct {
		index     int
		name      string
		replicas  int
		resources map[string]object
	}{
		{1, "ate-api-server", 3, map[string]object{"ate-api-server": {"requests": object{"cpu": "500m"}, "limits": object{"memory": "1Gi"}}}},
		{2, "atenet-router", 2, map[string]object{
			"atenet-router": {"requests": object{"cpu": "250m"}},
			"envoy":         {"limits": object{"memory": "512Mi"}},
		}},
	} {
		doc := manifestObject(t, []byte(renders[0][workload.index]), "Deployment", workload.name)
		spec, err := child(doc, "spec")
		if err != nil || spec["replicas"] != workload.replicas {
			t.Fatalf("%s replicas after repeated install: %v, %v", workload.name, spec, err)
		}
		pod, err := nested(spec, "template", "spec")
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range workload.resources {
			container, err := named(pod["containers"], name)
			if err != nil || !reflect.DeepEqual(container["resources"], want) {
				t.Fatalf("%s resources after repeated install: %v, %v", name, container["resources"], err)
			}
		}
	}
}

func TestPreparedSourceReplacesLegacyDemoCache(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	const previousMarker = `{"old":"cache without the Azure ActorTemplate adaptation"}`
	var markers []string
	for _, suffix := range []string{"", "-v2"} {
		legacy := filepath.Join(c.workspace(), "source-"+fingerprint(c, i)+suffix)
		if err := os.MkdirAll(legacy, 0700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(legacy, ".substrate-aks-prepared.json")
		if err := os.WriteFile(marker, []byte(previousMarker), 0600); err != nil {
			t.Fatal(err)
		}
		markers = append(markers, marker)
	}
	want := errors.New("stopped before fetching fresh source")
	r := script(t, reply{contains: "git init", err: want})
	if _, err := (App{Config: c, Runner: r}).prepare(t.Context(), i); !errors.Is(err, want) {
		t.Fatalf("prepare reused or rejected the legacy cache instead of fetching fresh source: %v", err)
	}
	for _, marker := range markers {
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != previousMarker {
			t.Fatalf("prepare modified the previous managed checkout: %s, %v", data, err)
		}
	}
}

func TestPreparedSourceCreatesNewGenerationWithoutChangingOld(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	fp := fingerprint(c, i)
	oldRoots := []string{
		filepath.Join(c.workspace(), "source-"+fp),
		filepath.Join(c.workspace(), "source-"+fp+"-v2"),
	}
	for _, root := range oldRoots {
		if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		p := prepared{Fingerprint: fp, Files: make(map[string]string)}
		for _, rel := range adaptedSourcePaths[:6] {
			path := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			data := []byte("prior generation: " + rel)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			p.Files[rel] = checksum(data)
		}
		marker, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".substrate-aks-prepared.json"), marker, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "nested/workload.yaml"), []byte("original content\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() map[string]string {
		t.Helper()
		result := make(map[string]string)
		for _, root := range oldRoots {
			err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				value := info.Mode().String()
				if !entry.IsDir() {
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					value += ":" + checksum(data)
				}
				result[path] = value
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before := snapshot()
	api := strings.Replace(string(workloadFixture(false)), "- name: atelet\n        image:", "- name: ate-api-server\n        image:", 1)
	fixtures := map[string]string{
		adaptedSourcePaths[0]: string(workloadFixture(true)),
		adaptedSourcePaths[1]: api,
		adaptedSourcePaths[2]: `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: ate-system
spec:
  template:
    spec:
      containers:
      - name: postgres
        image: example.invalid/postgres
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
`,
		adaptedSourcePaths[3]: `apiVersion: v1
kind: ConfigMap
metadata:
  name: ate-otel-config
  namespace: ate-system
data:
  example: original
`,
		adaptedSourcePaths[4]: `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../ate-api-server.yaml
- ../ate-idp-server.yaml
- ../ate-controller.yaml
- ../atelet.yaml
- ../atenet-router.yaml
- ../atenet-router-monitoring.yaml
- ../ate-otel-config.yaml
`,
		adaptedSourcePaths[5]: `apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: counter-microvm
  namespace: ate-demo-counter-microvm
spec:
  template:
    nodeSelector:
      existing: retained
`,
		adaptedSourcePaths[6]: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: atenet-router
  namespace: ate-system
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: atenet-router
        image: example.invalid/router
      - name: envoy
        image: example.invalid/envoy
`,
	}
	sourceReplies := fetchReplies(c.Substrate.Source)
	sourceReplies[len(sourceReplies)-1].effect = func(cmd Command) {
		writeAssetFixture(t, cmd.Dir, t.TempDir())
		for rel, content := range fixtures {
			path := filepath.Join(cmd.Dir, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	pluginReplies := fetchReplies(c.Substrate.PluginSource)
	pluginReplies[len(pluginReplies)-1].effect = func(cmd Command) {
		if err := os.CopyFS(cmd.Dir, os.DirFS("testdata/plugin")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cmd.Dir, "cmd/azure-blob-objectstore-plugin"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	replies := append(sourceReplies, pluginReplies...)
	replies = append(replies,
		reply{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
		reply{contains: "git diff --name-only HEAD"},
		reply{contains: "git ls-files --others -z --", output: ".substrate-aks-prepared.json\x00"})
	app := App{Config: c, Runner: script(t, replies...)}
	root, err := app.prepare(t.Context(), i)
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(c.workspace(), "source-"+fp+"-v3") {
		t.Fatalf("unexpected generation: %s", root)
	}
	marker, err := os.ReadFile(filepath.Join(root, ".substrate-aks-prepared.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p prepared
	if err := json.Unmarshal(marker, &p); err != nil {
		t.Fatal(err)
	}
	if p.Fingerprint != fp || len(p.Files) != len(adaptedSourcePaths) {
		t.Fatalf("invalid prepared marker: %+v", p)
	}
	for _, rel := range adaptedSourcePaths {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || p.Files[rel] != checksum(data) {
			t.Fatalf("missing or inconsistent adapted file %s: %v", rel, err)
		}
	}
	router, err := os.ReadFile(filepath.Join(root, adaptedSourcePaths[6]))
	if err != nil || string(router) != fixtures[adaptedSourcePaths[6]] {
		t.Fatalf("new generation changed the unset router: %s, %v", router, err)
	}
	if cached, err := app.prepare(t.Context(), i); err != nil || cached != root {
		t.Fatalf("new generation cannot be reused: %q, %v", cached, err)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("old generations changed: before=%v after=%v", before, after)
	}
}

func TestUpstreamRefreshesOwnedRegistryCredentials(t *testing.T) {
	for _, component := range []string{"ate-system", "atelet", "apiserver", "demo counter-microvm"} {
		t.Run(component, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images.Repository = "substratetest123.azurecr.io/substrate"
			r := script(t, accountReply(c), reply{contains: "group exists", output: "true"},
				ownerReply(c), registryReply(c), acrLoginReply(),
				reply{contains: "go run ./cmd/ate-setup deploy " + component})
			args := append([]string{"deploy"}, strings.Fields(component)...)
			if err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", args...); err != nil {
				t.Fatal(err)
			}
			login, deploy := r.calls[len(r.calls)-2], r.calls[len(r.calls)-1]
			if !strings.Contains(strings.Join(deploy.Args, " "), "--ingress-allowed-spiffe-ids "+routerClientSPIFFEID) {
				t.Fatal("kubectl ate proxy cannot reach actors through atenet-router")
			}
			dockerEnv := "DOCKER_CONFIG=" + filepath.Join(c.workspace(), "docker")
			for _, call := range []Command{deploy} {
				found := false
				for _, entry := range call.Env {
					found = found || entry == dockerEnv
				}
				if !found {
					t.Fatalf("deploy must use isolated registry credentials: %+v", call)
				}
			}
			if !strings.Contains(strings.Join(login.Args, " "), "--name substratetest123 --subscription "+c.Azure.SubscriptionID) {
				t.Fatalf("login lost registry or subscription: %v", login.Args)
			}
			if !login.SensitiveOutput || !strings.Contains(strings.Join(login.Args, " "), "--expose-token") {
				t.Fatal("login requires Docker or exposes the ACR token")
			}
		})
	}
}

func TestUpstreamRegistryLoginFailureStopsDeployment(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images.Repository = "substratetest123.azurecr.io/substrate"
	want := errors.New("Azure login expired")
	r := script(t, accountReply(c), reply{contains: "group exists", output: "true"},
		ownerReply(c), registryReply(c), reply{contains: "acr login", err: want})
	err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", "deploy", "ate-system")
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "ACR login failed") {
		t.Fatalf("login failure was not propagated: %v", err)
	}
}

func TestUpstreamPreservesExternalRegistryCredentials(t *testing.T) {
	for _, repo := range []string{"ghcr.io/example/substrate", "external.azurecr.io/substrate"} {
		t.Run(repo, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images.Repository = repo
			var replies []reply
			if strings.Contains(repo, ".azurecr.io/") {
				replies = append(replies, accountReply(c), reply{contains: "group exists", output: "true"},
					ownerReply(c), registryReply(c))
			}
			r := script(t, append(replies, reply{contains: "go run ./cmd/ate-setup deploy ate-system"})...)
			if err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", "deploy", "ate-system"); err != nil {
				t.Fatal(err)
			}
			for _, entry := range r.calls[len(r.calls)-1].Env {
				if strings.HasPrefix(entry, "DOCKER_CONFIG=") {
					t.Fatal("external registry credentials replaced with the owned registry's config")
				}
			}
		})
	}
}
