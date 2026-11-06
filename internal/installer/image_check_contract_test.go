//go:build integration

package installer

import (
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestPinnedImageCheckContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := filepath.Join(t.TempDir(), "source")
	if output, err := exec.Command("git", "clone", "--quiet", "--shared", root, dest).CombinedOutput(); err != nil {
		t.Fatalf("copying pinned source: %v\n%s", err, output)
	}
	if err := prepareHelper(dest); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "ate-setup")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/ate-setup")
	build.Dir = dest
	build.Env = cleanEnvironment(os.Environ())
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building pinned image checker: %v\n%s", err, output)
	}
	inventory, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "cmd/ate-setup/internal/images/images.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Releases publish the released packages and envoy-dataplane; the installer
	// adds the counter demo and the plugin.
	want := map[string]bool{"envoy-dataplane": true, "counter": true, "plugin": true}
	ast.Inspect(inventory, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "released" {
			return true
		}
		list, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("publisher inventory is no longer a literal list")
		}
		for _, element := range list.Elts {
			literal, ok := element.(*ast.BasicLit)
			if !ok {
				t.Fatal("publisher inventory entry is not a literal")
			}
			pkg, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			want[path.Base(pkg)] = true
		}
		return false
	})
	if len(want) <= 3 {
		t.Fatal("could not read the pinned publisher's inventory")
	}
	for _, tc := range []struct {
		name, image, status string
		httpStatus          int
		disconnect          bool
		wrongDigest         bool
		tokenFailure        bool
	}{
		{name: "valid full inventory and mixed registry credentials", status: "available"},
		{name: "empty recreated registry", image: "ate-idp-server", status: "missing", httpStatus: http.StatusNotFound},
		{name: "missing podcertcontroller", image: "podcertcontroller", status: "missing", httpStatus: http.StatusNotFound},
		{name: "missing exact plugin digest", image: "plugin", status: "missing", httpStatus: http.StatusNotFound},
		{name: "authentication failure", image: "podcertcontroller", status: "authentication", httpStatus: http.StatusUnauthorized},
		{name: "authorization failure", image: "podcertcontroller", status: "authorization", httpStatus: http.StatusForbidden},
		{name: "network failure", image: "podcertcontroller", status: "network", disconnect: true},
		{name: "registry failure", image: "podcertcontroller", status: "registry-error", httpStatus: http.StatusInternalServerError},
		{name: "wrong plugin digest", image: "plugin", status: "registry-error", wrongDigest: true},
		{name: "token endpoint 404 is not a missing image", image: "ate-idp-server", status: "registry-error", tokenFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const secret = "synthetic-credential-must-not-appear"
			const tag = "contract-images"
			pluginDigest := "sha256:" + strings.Repeat("a", 64)
			var mu sync.Mutex
			seen := map[string]bool{}
			handler := func(user string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if tc.tokenFailure && r.URL.Path == "/token" {
						w.WriteHeader(http.StatusNotFound)
						w.Write([]byte(secret))
						return
					}
					if r.URL.Path == "/v2/" && r.Method == http.MethodGet {
						if tc.tokenFailure {
							w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token?secret=`+secret+`",service="registry.local"`)
							w.WriteHeader(http.StatusUnauthorized)
						}
						return
					}
					if r.Method != http.MethodHead {
						t.Errorf("image verification made a non-HEAD request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					username, password, ok := r.BasicAuth()
					if !ok || username != user || password != secret {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					parts := strings.Split(r.URL.Path, "/")
					image := parts[len(parts)-3]
					mu.Lock()
					seen[image] = true
					mu.Unlock()
					if image == tc.image {
						if tc.disconnect {
							connection, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							connection.Close()
							return
						}
						if tc.httpStatus != 0 {
							w.WriteHeader(tc.httpStatus)
							w.Write([]byte(secret))
							return
						}
					}
					digest := "sha256:" + strings.Repeat("b", 64)
					if image == "plugin" && !tc.wrongDigest {
						digest = pluginDigest
					}
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Content-Length", "123")
					w.Header().Set("Docker-Content-Digest", digest)
				}
			}
			owned := httptest.NewServer(handler("owned"))
			defer owned.Close()
			external := httptest.NewServer(handler("external"))
			defer external.Close()
			ownedHost := strings.TrimPrefix(owned.URL, "http://")
			externalHost := strings.TrimPrefix(external.URL, "http://")
			dockerConfig := func(host, user string) string {
				t.Helper()
				dir := t.TempDir()
				data, err := json.Marshal(map[string]any{"auths": map[string]any{
					host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))},
				}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				return dir
			}
			repository := ownedHost + "/substrate"
			plugin := externalHost + "/plugin@" + pluginDigest
			check := exec.CommandContext(t.Context(), binary, "publish", "check-images",
				repository, tag, plugin, ownedHost, dockerConfig(ownedHost, "owned"),
				"--no-dev-env", "--no-report-defaults", "--kubeconfig", "/not-a-kubeconfig", "--context", "not-a-cluster")
			check.Dir = dest
			check.Env = append(cleanEnvironment(os.Environ()), "DOCKER_CONFIG="+dockerConfig(externalHost, "external"),
				"VERSION="+tag, "NO_DEV_ENV=1")
			output, err := check.CombinedOutput()
			if err != nil {
				t.Fatalf("checker failed before reporting registry availability: %v\n%s", err, output)
			}
			if strings.Contains(string(output), secret) {
				t.Fatal("checker exposed registry credentials or error-body contents")
			}
			var report struct {
				Status   string `json:"status"`
				Image    string `json:"image"`
				Checked  int    `json:"checked"`
				Expected int    `json:"expected"`
			}
			if err := json.Unmarshal(output, &report); err != nil {
				t.Fatalf("checker output was not a structured result: %v\n%s", err, output)
			}
			if report.Status != tc.status || report.Expected != len(want) {
				t.Fatalf("wrong registry classification or publisher inventory: %+v", report)
			}
			mu.Lock()
			actual := make(map[string]bool, len(seen))
			for image := range seen {
				actual[image] = true
			}
			mu.Unlock()
			if tc.status == "available" {
				if err := imageCheckResult(output); err != nil || !reflect.DeepEqual(actual, want) {
					t.Fatalf("valid image set was not fully checked: %v; checked=%v, expected=%v", err, actual, want)
				}
			} else if err := imageCheckResult(output); err == nil || (!tc.tokenFailure && !actual[tc.image]) {
				t.Fatalf("registry failure became successful validation: %v; %+v", err, report)
			}
		})
	}
	kubeconfig := filepath.Join(t.TempDir(), "invalid-kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("not kubeconfig YAML ["), 0600); err != nil {
		t.Fatal(err)
	}
	deploy := exec.CommandContext(t.Context(), binary, "deploy", "ate-system", "--no-dev-env", "--no-report-defaults",
		"--kubeconfig", kubeconfig, "--context", "not-a-cluster")
	deploy.Dir = dest
	deploy.Env = cleanEnvironment(os.Environ())
	output, err := deploy.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "while reading kubeconfig") {
		t.Fatalf("image checker bypassed normal deployment startup guards: %v\n%s", err, output)
	}
}
