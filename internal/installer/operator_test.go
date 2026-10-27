package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionCarriesExplicitSnapshotReaderOptIn(t *testing.T) {
	for _, reader := range []string{"", "77777777-7777-4777-8777-777777777777"} {
		t.Run("reader="+reader, func(t *testing.T) {
			c := testConfig(t)
			c.Azure.SnapshotReaderObjectID = reader
			r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
				reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
			if err := (App{Config: c, Runner: r}).Provision(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(c.workspace(), "infra/parameters.json"))
			if err != nil {
				t.Fatal(err)
			}
			var parameters struct {
				Parameters map[string]struct {
					Value any `json:"value"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(data, &parameters); err != nil {
				t.Fatal(err)
			}
			if parameters.Parameters["snapshotReaderObjectId"].Value != reader {
				t.Fatalf("provisioning lost the explicit reader choice: %s", data)
			}
		})
	}
}
