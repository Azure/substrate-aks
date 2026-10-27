package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func kubeconfigFixture(name string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: %s
contexts:
- name: %s
  context: {cluster: aks, user: operator}
clusters:
- name: aks
  cluster: {server: "https://aks.example.invalid"}
users:
- name: operator
  user: {token: "synthetic-token"}
`, name, name))
}

func TestCredentialsRefreshIsAtomic(t *testing.T) {
	wantErr := errors.New("Azure session expired")
	for _, tc := range []struct {
		name string
		data func(string) []byte
		err  error
		ok   bool
	}{
		{"success", kubeconfigFixture, nil, true},
		{"partial write then authentication failure", func(string) []byte { return []byte("partial-secret") }, wantErr, false},
		{"empty success", func(string) []byte { return nil }, nil, false},
		{"invalid yaml", func(string) []byte { return []byte("token: [secret") }, nil, false},
		{"wrong context", func(string) []byte { return kubeconfigFixture("other") }, nil, false},
		{"empty user", func(name string) []byte {
			return []byte(strings.Replace(string(kubeconfigFixture(name)), `user: {token: "synthetic-token"}`, `user: {}`, 1))
		}, nil, false},
		{"dangling cluster", func(name string) []byte {
			return []byte(strings.Replace(string(kubeconfigFixture(name)), "cluster: aks", "cluster: missing", 1))
		}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			a := App{Config: c}
			if err := os.MkdirAll(c.workspace(), 0700); err != nil {
				t.Fatal(err)
			}
			old := "previous-working-credentials"
			if err := os.WriteFile(a.kubeconfig(), []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
			a.Runner = script(t, reply{contains: "aks get-credentials", err: tc.err, effect: func(cmd Command) {
				data, err := os.ReadFile(a.kubeconfig())
				if err != nil || string(data) != old {
					t.Fatal("refresh destroyed working credentials before Azure returned")
				}
				for i, arg := range cmd.Args {
					if arg == "--file" {
						path := cmd.Args[i+1]
						if path == a.kubeconfig() {
							t.Fatal("Azure was given the existing kubeconfig")
						}
						if err := os.WriteFile(path, tc.data(c.AKS.ClusterName), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Chmod(path, 0644); err != nil {
							t.Fatal(err)
						}
						return
					}
				}
				t.Fatal("refresh did not pass --file")
			}})
			err := a.credentials(t.Context())
			if (err == nil) != tc.ok || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("refresh result = %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "partial-secret") || strings.Contains(err.Error(), "token:")) {
				t.Fatal("refresh disclosed credential contents")
			}
			want := old
			if tc.ok {
				want = string(tc.data(c.AKS.ClusterName))
			}
			data, err := os.ReadFile(a.kubeconfig())
			if err != nil || string(data) != want {
				t.Fatalf("incorrect kubeconfig after refresh: %v", err)
			}
			info, err := os.Stat(a.kubeconfig())
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("kubeconfig permissions changed: %v", err)
			}
			temporary, err := filepath.Glob(filepath.Join(c.workspace(), ".kubeconfig-*"))
			if err != nil || len(temporary) != 0 {
				t.Fatalf("temporary credentials retained: %v, %v", temporary, err)
			}
		})
	}
}
