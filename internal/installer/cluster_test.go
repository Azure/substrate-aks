package installer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestInstallWaitsForOperatorAccessBeforeOwnershipCheck(t *testing.T) {
	c := testConfig(t)
	c.AKS.Operator = &OperatorConfig{ObjectID: c.Azure.AssetPublisherObjectID, PrincipalType: "User"}
	outputs, err := json.Marshal(testInfrastructure())
	if err != nil {
		t.Fatal(err)
	}

	r := script(t,
		reply{contains: "group exists", output: "true"}, ownerReply(c),
		reply{contains: "deployment sub show", output: string(outputs)},
		reply{contains: "aks get-credentials"},
		reply{contains: "kubelogin convert-kubeconfig", effect: func(cmd Command) {
			if err := os.WriteFile(cmd.Args[len(cmd.Args)-1], entraKubeconfigFixture(c.AKS.ClusterName), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		reply{contains: "create -f - --output=json", output: `{"status":{"allowed":false,"reason":"not ready"}}`},
		reply{contains: "create -f - --output=json", output: `{"status":{"allowed":true}}`},
		reply{contains: "get namespace", output: "{}"},
		reply{contains: "get configmap", output: `{"data":{"installationId":"foreign","fingerprint":"foreign","phase":"deployed"}}`},
	)
	err = (App{Config: c, Runner: r}).Install(t.Context(), "ate-system")
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("foreign installation was not refused after access was granted: %v", err)
	}
}

func TestCheckDoesNotReacquireCallerLock(t *testing.T) {
	c := testConfig(t)
	lock, err := Lock(c)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	outputs, err := json.Marshal(testInfrastructure())
	if err != nil {
		t.Fatal(err)
	}
	r := script(t,
		reply{contains: "group exists", output: "true"}, ownerReply(c),
		reply{contains: "deployment sub show", output: string(outputs)},
		reply{contains: "aks get-credentials"},
		reply{contains: "get --raw /version", output: `{"gitVersion":"v1.37.1"}`},
		reply{contains: "api-resources", output: "podcertificaterequests.certificates.k8s.io\nclustertrustbundles.certificates.k8s.io\n"},
		reply{contains: "wait node"},
		reply{contains: "get nodes", output: `{"items":[{"metadata":{"labels":{"substrate-aks/runtime":"microvm"}},"status":{"nodeInfo":{"kubeletVersion":"v1.37.1","architecture":"amd64","operatingSystem":"linux"}}}]}`},
		reply{contains: "get namespace ate-system"},
	)
	if err := (App{Config: c, Runner: r}).Check(t.Context(), false); err != nil {
		t.Fatalf("check conflicted with the caller's lock: %v", err)
	}
}
