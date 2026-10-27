package installer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func clientFixture(t *testing.T, c Config) (string, []reply) {
	t.Helper()
	i := testInfrastructure()
	fp := fingerprint(c, i)
	root := filepath.Join(c.workspace(), "source-"+fp+"-v2")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := prepared{Fingerprint: fp, Files: map[string]string{}}
	for _, name := range []string{"atelet.yaml", "api.yaml", "postgres.yaml", "otel.yaml", "base.yaml", "demo.yaml"} {
		data := []byte("prepared " + name)
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
	outputs, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	return root, []reply{
		{contains: "group exists", output: "true"}, ownerReply(c),
		{contains: "deployment sub list", output: string(outputs)},
		{contains: "aks get-credentials"},
		{contains: "get namespace", output: "{}"},
		{contains: "get configmap", output: `{"data":{"installationId":"` + c.Installation.ID + `","fingerprint":"` + fp + `","phase":"deployed"}}`},
		{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
		{contains: "git diff --name-only HEAD"},
		{contains: "git ls-files --others -z --", output: ".substrate-aks-prepared.json\x00"},
	}
}

func TestSetupClientInstallsPinnedClientInGoBinaryDirectory(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "first GOPATH bin", true: "explicit GOBIN"}[explicit], func(t *testing.T) {
			c := testConfig(t)
			root, replies := clientFixture(t, c)
			first, second := t.TempDir(), t.TempDir()
			goEnv := map[string]string{"GOBIN": "", "GOPATH": first + string(os.PathListSeparator) + second}
			binDir := filepath.Join(first, "bin")
			if explicit {
				binDir = t.TempDir()
				goEnv["GOBIN"] = binDir
			}
			data, err := json.Marshal(goEnv)
			if err != nil {
				t.Fatal(err)
			}
			r := script(t, append(replies,
				reply{contains: "go env -json GOBIN GOPATH", output: string(data)},
				reply{contains: "go install ./cmd/kubectl-ate"})...)
			got, err := (App{Config: c, Runner: r}).SetupClient(t.Context())
			want := ClientSetup{Kubeconfig: filepath.Join(c.workspace(), "kubeconfig"), Context: c.AKS.ClusterName, BinDir: binDir}
			if err != nil || got != want {
				t.Fatalf("client target = %+v, %v; want %+v", got, err, want)
			}
			for _, call := range r.calls {
				if call.Args[0] == "go" && (call.Dir != root ||
					!reflect.DeepEqual(call.Env, []string{"GOOS=" + runtime.GOOS, "GOARCH=" + runtime.GOARCH}) ||
					call.ShowOutput || len(call.Input) != 0) {
					t.Fatalf("client installation did not use the verified source and host target: %+v", call)
				}
				if call.Args[0] == "kubectl" &&
					!reflect.DeepEqual(call.Args[:5], []string{"kubectl", "--kubeconfig", got.Kubeconfig, "--context", got.Context}) {
					t.Fatalf("ownership check targeted the wrong cluster: %+v", call)
				}
			}
		})
	}
}

func TestSetupClientPropagatesGoFailures(t *testing.T) {
	for _, command := range []string{"go env", "go install"} {
		t.Run(command, func(t *testing.T) {
			c := testConfig(t)
			_, replies := clientFixture(t, c)
			if command == "go install" {
				data, err := json.Marshal(map[string]string{"GOBIN": t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				replies = append(replies, reply{contains: "go env", output: string(data)})
			}
			want := errors.New("Go command failed")
			r := script(t, append(replies, reply{contains: command, err: want})...)
			got, err := (App{Config: c, Runner: r}).SetupClient(t.Context())
			if !errors.Is(err, want) || got != (ClientSetup{}) {
				t.Fatalf("Go failure returned a successful handoff: %+v, %v", got, err)
			}
		})
	}
}

func TestSetupClientRejectsInvalidGoBinaryDirectory(t *testing.T) {
	for _, output := range []string{"not json", "{}", "null", `{"GOBIN":"relative"}`, `{"GOPATH":"relative"}`} {
		t.Run(output, func(t *testing.T) {
			c := testConfig(t)
			_, replies := clientFixture(t, c)
			got, err := (App{Config: c, Runner: script(t, append(replies, reply{contains: "go env", output: output})...)}).SetupClient(t.Context())
			if err == nil || got != (ClientSetup{}) {
				t.Fatalf("invalid Go directory produced a client handoff: %+v, %v", got, err)
			}
		})
	}
}

type clientInstallRunner struct{ *scriptedRunner }

func (r clientInstallRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if c.Args[0] == "go" {
		return (ExecRunner{}).Run(ctx, c)
	}
	return r.scriptedRunner.Run(ctx, c)
}

func TestSetupClientInstallsExecutableWithGoTool(t *testing.T) {
	c := testConfig(t)
	root, replies := clientFixture(t, c)
	binDir := filepath.Join(t.TempDir(), "bin")
	t.Setenv("GOBIN", binDir)
	t.Setenv("GOTOOLCHAIN", "local")
	if err := os.MkdirAll(filepath.Join(root, "cmd", "kubectl-ate"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"go.mod":                  "module example.invalid/client-test\n\ngo 1.26.0\n",
		"cmd/kubectl-ate/main.go": "package main\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := (App{Config: c, Runner: clientInstallRunner{script(t, replies...)}}).SetupClient(t.Context())
	if err != nil || got.BinDir != binDir {
		t.Fatalf("Go installation failed: %+v, %v", got, err)
	}
	name := "kubectl-ate"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	info, err := os.Stat(filepath.Join(binDir, name))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		t.Fatalf("client was not installed as an executable in GOBIN: %v, %v", info, err)
	}
}

func TestSetupClientRequiresCompletedOwnedInstallation(t *testing.T) {
	for _, phase := range []string{"installing:atelet", "deployed"} {
		t.Run(phase, func(t *testing.T) {
			c := testConfig(t)
			_, replies := clientFixture(t, c)
			// The install record is checked before the managed source is touched.
			replies = replies[:6]
			record := &replies[len(replies)-1]
			if phase == "deployed" {
				record.output = strings.ReplaceAll(record.output, c.Installation.ID, "another-installation")
			} else {
				record.output = strings.ReplaceAll(record.output, "deployed", phase)
			}
			got, err := (App{Config: c, Runner: script(t, replies...)}).SetupClient(t.Context())
			if err == nil || got != (ClientSetup{}) {
				t.Fatalf("unowned or incomplete installation produced a client: %+v, %v", got, err)
			}
		})
	}
}
