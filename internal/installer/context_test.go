package installer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func contextApp(t *testing.T) App {
	t.Helper()
	c := testConfig(t)
	a := App{Config: c}
	if err := os.MkdirAll(c.workspace(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.kubeconfig(), kubeconfigFixture(c.AKS.ClusterName), 0600); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestContextImportPreservesUnrelatedConfigAndPrivateCredentials(t *testing.T) {
	a := contextApp(t)
	source, err := decodeContextFile(kubeconfigFixture(a.Config.AKS.ClusterName))
	if err != nil {
		t.Fatal(err)
	}
	source["users"] = append(source["users"].([]any), object{"name": "unrelated-private-user", "user": object{"token": "synthetic-other-token"}})
	sourceData, err := yaml.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.kubeconfig(), sourceData, 0600); err != nil {
		t.Fatal(err)
	}
	old := strings.ReplaceAll(string(kubeconfigFixture("previous-context")), "aks", "previous-cluster")
	old = strings.ReplaceAll(old, "name: operator", "name: previous-user")
	old = strings.ReplaceAll(old, "user: operator", "user: previous-user")
	old += "preferences: {colors: true}\nextensions:\n- name: preserved\n  extension: {custom: value}\n"
	path := filepath.Join(t.TempDir(), "kube", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(old), 0640); err != nil {
		t.Fatal(err)
	}
	before, err := decodeContextFile([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	private, err := os.ReadFile(a.kubeconfig())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		selection, err := PlanContextSwitch(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.SelectContext(t.Context(), selection); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateKubeconfig(data, a.Config.AKS.ClusterName); err != nil {
			t.Fatal(err)
		}
		after, err := decodeContextFile(data)
		if err != nil {
			t.Fatal(err)
		}
		if contextEntry(after, "users", "unrelated-private-user") != nil {
			t.Fatal("handoff imported an unrelated private credential")
		}
		for _, field := range []string{"preferences", "extensions"} {
			if !reflect.DeepEqual(before[field], after[field]) {
				t.Fatalf("import discarded unrelated %s", field)
			}
		}
		for field, name := range map[string]string{"contexts": "previous-context", "clusters": "previous-cluster", "users": "previous-user"} {
			if !reflect.DeepEqual(contextEntry(before, field, name), contextEntry(after, field, name)) {
				t.Fatalf("import changed unrelated %s entry", field)
			}
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("imported kubeconfig is not private: %v", err)
		}
	}
	after, err := os.ReadFile(a.kubeconfig())
	if err != nil || !bytes.Equal(private, after) {
		t.Fatalf("context selection changed private credentials: %v", err)
	}
}

func TestContextImportRefusesCollisionsAndChangedDestination(t *testing.T) {
	for _, mode := range []string{"conflicting cluster", "conflicting user", "changed while prompting", "created while prompting"} {
		t.Run(mode, func(t *testing.T) {
			a := contextApp(t)
			path := filepath.Join(t.TempDir(), "config")
			before := []byte("apiVersion: v1\nkind: Config\n")
			if mode == "conflicting cluster" {
				before = bytes.Replace(kubeconfigFixture(a.Config.AKS.ClusterName),
					[]byte("https://aks.example.invalid"), []byte("https://another-cluster.example.invalid"), 1)
			}
			if mode == "conflicting user" {
				before = bytes.Replace(kubeconfigFixture(a.Config.AKS.ClusterName),
					[]byte("synthetic-token"), []byte("different-user-token"), 1)
			}
			if mode != "created while prompting" {
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			selection, err := PlanContextSwitch(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(mode, "conflicting") {
				before = append(before, []byte("current-context: changed\n")...)
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.SelectContext(t.Context(), selection); err == nil {
				t.Fatal("unsafe context import succeeded")
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, data) {
				t.Fatalf("failed import changed destination: %v", err)
			}
			files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".substrate-context-*"))
			if err != nil || len(files) != 0 {
				t.Fatalf("temporary credentials retained: %v, %v", files, err)
			}
		})
	}
}

func TestContextPlanningRefusesUnsafeFilesAndAmbiguousEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	for _, data := range []string{
		"token: [malformed", "null", "apiVersion: v1\nkind: Config\n---\nkind: Secret\n",
		"apiVersion: v1\nkind: Config\ncontexts: [{name: repeated}, {name: repeated}]\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := PlanContextSwitch(path); err == nil {
			t.Fatalf("accepted malformed or ambiguous kubeconfig: %q", data)
		}
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{link, dir} {
		if _, err := PlanContextSwitch(destination); err == nil {
			t.Fatalf("accepted nonregular destination %s", destination)
		}
	}
	t.Setenv("KUBECONFIG", path+string(os.PathListSeparator)+link)
	if _, err := PlanContextSwitch(""); err == nil {
		t.Fatal("multi-file KUBECONFIG was silently flattened")
	}
	t.Setenv("KUBECONFIG", filepath.Join(dir, "new-config"))
	selection, err := PlanContextSwitch("")
	if err != nil || selection.Path != os.Getenv("KUBECONFIG") || selection.Previous != "" {
		t.Fatalf("single-file destination not resolved: %+v, %v", selection, err)
	}
	if _, err := os.Stat(selection.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planning created a destination before consent: %v", err)
	}
}

func TestContextImportRejectsPrivateFileAliases(t *testing.T) {
	a := contextApp(t)
	dir := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(a.Config.workspace(), dir); err != nil {
		t.Fatal(err)
	}
	selection, err := PlanContextSwitch(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SelectContext(t.Context(), selection); err == nil {
		t.Fatal("context selection replaced private credentials through a parent-directory alias")
	}
}

func TestContextImportRejectsRelativePrivateCredentialPaths(t *testing.T) {
	a := contextApp(t)
	config, err := decodeContextFile(kubeconfigFixture(a.Config.AKS.ClusterName))
	if err != nil {
		t.Fatal(err)
	}
	contextEntry(config, "clusters", "aks")["cluster"].(object)["certificate-authority"] = "relative-ca.crt"
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.kubeconfig(), data, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config")
	selection, err := PlanContextSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SelectContext(t.Context(), selection); err == nil {
		t.Fatal("relative credential path was broken by importing into a different directory")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed import created a file: %v", err)
	}
}
