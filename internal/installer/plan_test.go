package installer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestControlPlaneCapacityWarnings(t *testing.T) {
	cpu := resource.MustParse("3860m")
	memory := resource.MustParse("12188Mi")
	for _, tc := range []struct {
		name, cpu, memory, limitCPU, want string
		router                            bool
		count                             int
	}{
		{"empty", "", "", "", "", false, 1},
		{"cpu equals pool", "1930m", "", "", "", false, 1},
		{"cpu exceeds pool", "1931m", "", "", "aggregate requests exceed", false, 1},
		{"memory equals pool", "", "6094Mi", "", "", false, 1},
		{"memory exceeds pool", "", "6095Mi", "", "aggregate requests exceed", false, 1},
		{"limit becomes request", "", "", "2", "aggregate requests exceed", false, 1},
		{"request takes precedence", "1", "", "4", "", false, 1},
		{"cpu exceeds one node", "4", "", "", "a pod exceeds one node", true, 2},
		{"memory exceeds one node", "", "12Gi", "", "a pod exceeds one node", true, 2},
		{"pod equals one node", "3860m", "12188Mi", "", "", true, 2},
		{"larger pool", "1931m", "", "", "", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ResourceRequirements{Requests: map[string]string{}, Limits: map[string]string{}}
			if tc.cpu != "" {
				r.Requests["cpu"] = tc.cpu
			}
			if tc.memory != "" {
				r.Requests["memory"] = tc.memory
			}
			if tc.limitCPU != "" {
				r.Limits["cpu"] = tc.limitCPU
			}
			var sizing SizingConfig
			if tc.router {
				sizing.Router = &RouterSizing{Resources: &RouterResources{AtenetRouter: r}}
			} else {
				sizing.APIServer = &APIServerSizing{Resources: r}
			}
			got, err := capacityWarning(DefaultSystemVMSize, tc.count, cpu, memory, SubstrateConfig{Sizing: &sizing})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q; want %q", got, tc.want)
			}
			if got != "" {
				for _, piece := range []string{"WARNING CONTROL_PLANE_CAPACITY: system=", " requested=<", " estimated-allocatable=<", "; estimate excludes system/add-on pods; no fit guarantee"} {
					if !strings.Contains(got, piece) {
						t.Fatalf("warning lacks %q: %s", piece, got)
					}
				}
			}
		})
	}
}

func TestCapacityEstimateAcceptsDifferentPoolSizeAndCount(t *testing.T) {
	c := SubstrateConfig{Sizing: &SizingConfig{Router: &RouterSizing{
		Resources: &RouterResources{Envoy: &ResourceRequirements{Requests: map[string]string{"cpu": "8"}}},
	}}}
	cpu := resource.MustParse("7820m")
	memory := resource.MustParse("20Gi")
	warning, err := capacityWarning("Standard_D8s_v6", 3, cpu, memory, c)
	if err != nil || !strings.Contains(warning, "system=Standard_D8s_v6x3") ||
		!strings.Contains(warning, "a pod exceeds one node") || strings.Contains(warning, "aggregate requests exceed") {
		t.Fatalf("different pool estimate: %s, %v", warning, err)
	}
}

func TestControlPlaneCapacityCombinesRouterContainers(t *testing.T) {
	cpu := resource.MustParse("3860m")
	memory := resource.MustParse("12188Mi")
	c := SubstrateConfig{Sizing: &SizingConfig{Router: &RouterSizing{Resources: &RouterResources{
		AtenetRouter: &ResourceRequirements{Requests: map[string]string{"cpu": "2"}},
		Envoy:        &ResourceRequirements{Requests: map[string]string{"cpu": "2"}},
	}}}}
	warning, err := capacityWarning(DefaultSystemVMSize, 2, cpu, memory, c)
	if err != nil || !strings.Contains(warning, "a pod exceeds one node") || strings.Contains(warning, "aggregate requests exceed") {
		t.Fatalf("router containers were not summed: %q, %v", warning, err)
	}
}

func TestSystemCapacitySKUValidation(t *testing.T) {
	c := testConfig(t)
	a := App{Config: c}
	for _, tc := range []struct {
		name, cpus, memory string
		valid              bool
	}{
		{"valid", "4", "16", true},
		{"decimal memory", "4", "15.5", true},
		{"unsupported cpu", "3", "16", false},
		{"invalid cpu", "x", "16", false},
		{"missing memory", "4", "", false},
		{"invalid memory", "4", "-1", false},
		{"zero memory", "4", "0", false},
		{"maximum memory", "4", "65536", true},
		{"above maximum memory", "4", "65536.00000000000001", false},
		{"excess memory", "4", "99999999", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fmt.Sprintf(`[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":%q},{"name":"MemoryGB","value":%q}]}]`,
				DefaultSystemVMSize, tc.cpus, tc.memory)
			cpu, memory, err := a.systemCapacity([]byte(data), DefaultSystemVMSize)
			if (err == nil) != tc.valid {
				t.Fatalf("systemCapacity = %v; want valid %t", err, tc.valid)
			}
			if tc.name == "valid" && (cpu.Cmp(resource.MustParse("3860m")) != 0 ||
				memory.Cmp(resource.MustParse("12188Mi")) != 0) {
				t.Fatalf("wrong reservation: %s, %s", cpu.String(), memory.String())
			}
		})
	}
}

func TestSystemCapacityRejectsAmbiguousCapabilities(t *testing.T) {
	a := App{Config: testConfig(t)}
	for _, data := range []string{
		`[{"name":"Standard_D4s_v6","family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"16"}]}]`,
		`[{"name":"Standard_D4s_v6","family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"16"},{"name":"MemoryGB","value":"16"}]}]`,
		`[{"name":"Standard_D4s_v6","family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"16"}]},{"name":"Standard_D4s_v6","family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"16"}]}]`,
	} {
		if _, _, err := a.systemCapacity([]byte(data), DefaultSystemVMSize); err == nil {
			t.Fatalf("accepted ambiguous SKU capabilities: %s", data)
		}
	}
}

func TestSystemCapacityPreservesMemoryDecimalPrecision(t *testing.T) {
	data := fmt.Sprintf(`[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"0.400000000000000001"}]}]`, DefaultSystemVMSize)
	_, memory, err := (App{Config: testConfig(t)}).systemCapacity([]byte(data), DefaultSystemVMSize)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := memory.AsDec().String(), "217264947.200000000805306368"; got != want {
		t.Fatalf("capacity = %s, want %s", got, want)
	}
}

func TestSystemCapacityCPUReservations(t *testing.T) {
	a := App{Config: testConfig(t)}
	for _, tc := range []struct {
		cpus, reserved int64
	}{
		{1, 60}, {2, 100}, {4, 140}, {8, 180}, {16, 260}, {32, 420}, {64, 740},
	} {
		t.Run(fmt.Sprint(tc.cpus), func(t *testing.T) {
			data := fmt.Sprintf(`[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":%q},{"name":"MemoryGB","value":"16"}]}]`,
				DefaultSystemVMSize, fmt.Sprint(tc.cpus))
			cpu, _, err := a.systemCapacity([]byte(data), DefaultSystemVMSize)
			want := *resource.NewMilliQuantity(tc.cpus*1000-tc.reserved, resource.DecimalSI)
			if err != nil || cpu.Cmp(want) != 0 {
				t.Fatalf("capacity = %s, %v; want %s", cpu.String(), err, want.String())
			}
		})
	}
}

func TestSystemCapacityFractionalMemoryExact(t *testing.T) {
	c := testConfig(t)
	data := fmt.Sprintf(`[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"0.4"}]}]`, DefaultSystemVMSize)
	cpu, memory, err := (App{Config: c}).systemCapacity([]byte(data), DefaultSystemVMSize)
	if err != nil {
		t.Fatal(err)
	}
	want := resource.MustParse("217264947.2")
	if memory.Cmp(want) != 0 {
		t.Fatalf("fractional-byte estimate = %s, want %s", memory.String(), want.String())
	}
	two := 2
	for _, tc := range []struct {
		name     string
		request  string
		warn     bool
		count    int
		replicas *int
	}{
		{"below", "217264947.1", false, 1, nil},
		{"equal", "217264947.2", false, 1, nil},
		{"above", "217264947.3", true, 1, nil},
		{"two nodes below", "217264947.1", false, 2, &two},
		{"two nodes equal", "217264947.2", false, 2, &two},
		{"two nodes above", "217264947.3", true, 2, &two},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := SubstrateConfig{Sizing: &SizingConfig{Router: &RouterSizing{Replicas: tc.replicas, Resources: &RouterResources{
				Envoy: &ResourceRequirements{Requests: map[string]string{"memory": tc.request}},
			}}}}
			warning, err := capacityWarning(DefaultSystemVMSize, tc.count, cpu, memory, s)
			if err != nil || (warning != "") != tc.warn {
				t.Fatalf("request %s warning = %q, %v", tc.request, warning, err)
			}
			if tc.count == 2 && tc.warn && !strings.Contains(warning, "aggregate requests exceed") {
				t.Fatalf("multi-node aggregate overflow not reported: %s", warning)
			}
		})
	}
}

func TestControlPlaneCapacityFractionalAggregate(t *testing.T) {
	data := fmt.Sprintf(`[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"0.4"}]}]`, DefaultSystemVMSize)
	cpu, memory, err := (App{Config: testConfig(t)}).systemCapacity([]byte(data), DefaultSystemVMSize)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, apiRequest string
		warn             bool
	}{
		{"aggregate below", "108632473.5", false},
		{"aggregate equal", "108632473.6", false},
		{"aggregate above", "108632473.7", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := SubstrateConfig{Sizing: &SizingConfig{
				APIServer: &APIServerSizing{Resources: &ResourceRequirements{
					Requests: map[string]string{"memory": tc.apiRequest},
				}},
				Router: &RouterSizing{Resources: &RouterResources{
					Envoy: &ResourceRequirements{Requests: map[string]string{"memory": "217264947.2"}},
				}},
			}}
			warning, err := capacityWarning(DefaultSystemVMSize, 2, cpu, memory, s)
			if err != nil || (warning != "") != tc.warn {
				t.Fatalf("aggregate request warning = %q, %v", warning, err)
			}
			if tc.warn && (!strings.Contains(warning, "aggregate requests exceed") || strings.Contains(warning, "a pod exceeds")) {
				t.Fatalf("expected aggregate-only warning: %s", warning)
			}
		})
	}
}

func TestControlPlaneCapacityFractionalExactEquality(t *testing.T) {
	c := testConfig(t)
	data := fmt.Sprintf(`[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":"4"},{"name":"MemoryGB","value":"0.4"}]}]`, DefaultSystemVMSize)
	cpu, memory, err := (App{Config: c}).systemCapacity([]byte(data), DefaultSystemVMSize)
	if err != nil {
		t.Fatal(err)
	}
	// 0.4 GiB * 0.75 - 100 MiB = 217264947.2 bytes.
	s := SubstrateConfig{Sizing: &SizingConfig{Router: &RouterSizing{Resources: &RouterResources{
		Envoy: &ResourceRequirements{Requests: map[string]string{"memory": "217264947.2"}},
	}}}}
	warning, err := capacityWarning(DefaultSystemVMSize, 1, cpu, memory, s)
	if err != nil || warning != "" {
		t.Fatalf("exactly equal request must not warn: %q, %v", warning, err)
	}
}

func TestPlanUsesConfiguredSystemPool(t *testing.T) {
	for _, tc := range []struct {
		name, size, cpus, memory, nodeCPU, nodeMemory string
		count                                         int
		apiCPU, routerCPU, routerMemory               string
		routerReplicas                                int
		aggregate, pod, missing                       bool
	}{
		{name: "default equality", size: "Standard_D4s_v6", cpus: "4", memory: "16", nodeCPU: "3860m", nodeMemory: "12188Mi", count: 1, apiCPU: "1930m"},
		{name: "default aggregate over", size: "Standard_D4s_v6", cpus: "4", memory: "16", nodeCPU: "3860m", nodeMemory: "12188Mi", count: 1, apiCPU: "1931m", aggregate: true},
		{name: "two nodes aggregate equality", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 2, apiCPU: "7820m"},
		{name: "two nodes aggregate over", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 2, apiCPU: "7820m", routerCPU: "1m", aggregate: true},
		{name: "two nodes fit", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 2, apiCPU: "5"},
		{name: "same pods one node aggregate over", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 1, apiCPU: "5", aggregate: true},
		{name: "two nodes per-pod equality", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 2, routerCPU: "7820m"},
		{name: "two nodes per-pod over", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 2, routerCPU: "7821m", pod: true},
		{name: "one node per-pod over", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 1, routerCPU: "7821m", aggregate: true, pod: true},
		{name: "two nodes memory equality", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 2, routerMemory: "24476Mi", routerReplicas: 2},
		{name: "two nodes memory over", size: "Standard_D8s_v6", cpus: "8", memory: "32", nodeCPU: "7820m", nodeMemory: "24476Mi", count: 2, routerMemory: "24477Mi", routerReplicas: 2, aggregate: true, pod: true},
		{name: "larger SKU per-pod equality", size: "Standard_D32s_v6", cpus: "32", memory: "128", nodeCPU: "31580m", nodeMemory: "98204Mi", count: 2, routerCPU: "31580m"},
		{name: "larger SKU per-pod over", size: "Standard_D32s_v6", cpus: "32", memory: "128", nodeCPU: "31580m", nodeMemory: "98204Mi", count: 2, routerCPU: "31581m", pod: true},
		{name: "larger SKU aggregate equality", size: "Standard_D32s_v6", cpus: "32", memory: "128", nodeCPU: "31580m", nodeMemory: "98204Mi", count: 2, apiCPU: "31580m"},
		{name: "larger SKU aggregate over", size: "Standard_D32s_v6", cpus: "32", memory: "128", nodeCPU: "31580m", nodeMemory: "98204Mi", count: 2, apiCPU: "31580m", routerCPU: "1m", aggregate: true},
		{name: "configured SKU missing with default present", size: "Standard_D8s_v6", count: 2, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Azure.Location = "westus3"
			c.AKS.SystemPool = SystemPoolConfig{VMSize: tc.size, Count: tc.count}
			c.AKS.WorkerPool.VMSize = "Standard_D2s_v6"
			c.Substrate.Sizing = &SizingConfig{}
			if tc.apiCPU != "" {
				c.Substrate.Sizing.APIServer = &APIServerSizing{Resources: &ResourceRequirements{
					Requests: map[string]string{"cpu": tc.apiCPU},
				}}
			}
			if tc.routerCPU != "" || tc.routerMemory != "" {
				requests := map[string]string{}
				if tc.routerCPU != "" {
					requests["cpu"] = tc.routerCPU
				}
				if tc.routerMemory != "" {
					requests["memory"] = tc.routerMemory
				}
				router := &RouterSizing{Resources: &RouterResources{Envoy: &ResourceRequirements{Requests: requests}}}
				if tc.routerReplicas > 0 {
					router.Replicas = &tc.routerReplicas
				}
				c.Substrate.Sizing.Router = router
			}
			catalog := skuReply(DefaultSystemVMSize).output
			if !tc.missing {
				catalog = fmt.Sprintf(`{"value":[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":%q},{"name":"MemoryGB","value":%q}]}]}`,
					tc.size, tc.cpus, tc.memory)
			}
			r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
				reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`},
				reply{contains: "/providers/Microsoft.Compute/skus?", output: catalog})
			var warnings []string
			err := (App{Config: c, Runner: r, Warning: func(s string) { warnings = append(warnings, s) }}).Provision(t.Context(), true)
			if err != nil {
				t.Fatalf("plan failed: %v", err)
			}
			cmd := r.calls[len(r.calls)-1]
			var endpoint, query string
			for i := 0; i+1 < len(cmd.Args); i++ {
				switch cmd.Args[i] {
				case "--url":
					endpoint = cmd.Args[i+1]
				case "--query":
					query = cmd.Args[i+1]
				}
			}
			u, err := url.Parse(endpoint)
			if err != nil || u.Host != "management.azure.com" ||
				u.Path != "/subscriptions/"+c.Azure.SubscriptionID+"/providers/Microsoft.Compute/skus" ||
				u.Query().Get("$filter") != "location eq '"+c.Azure.Location+"'" ||
				!strings.Contains(strings.Join(cmd.Args, " "), "--subscription "+c.Azure.SubscriptionID) ||
				!strings.Contains(query, "name=='"+c.AKS.SystemPool.VMSize+"' || name=='"+c.AKS.WorkerPool.VMSize+"'") {
				t.Fatalf("lookup did not use configured subscription, region and SKUs: %v", cmd.Args)
			}
			if tc.missing {
				if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "WARNING CONTROL_PLANE_CAPACITY_UNKNOWN: ") {
					t.Fatalf("missing configured SKU must be unknown: %v", warnings)
				}
				return
			}
			if !tc.aggregate && !tc.pod {
				if len(warnings) != 0 {
					t.Fatalf("unexpected capacity warning: %v", warnings)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("expected one warning: %v", warnings)
			}
			cpu, memory := resource.MustParse(tc.nodeCPU), resource.MustParse(tc.nodeMemory)
			cpu.Mul(int64(tc.count))
			memory.Mul(int64(tc.count))
			memory.Format = resource.DecimalSI
			want := fmt.Sprintf("system=%sx%d requested=<", tc.size, tc.count)
			estimate := fmt.Sprintf("estimated-allocatable=<%s,%s>", cpu.String(), memory.String())
			if !strings.HasPrefix(warnings[0], "WARNING CONTROL_PLANE_CAPACITY: ") ||
				!strings.Contains(warnings[0], want) || !strings.Contains(warnings[0], estimate) ||
				strings.Contains(warnings[0], "aggregate requests exceed") != tc.aggregate ||
				strings.Contains(warnings[0], "a pod exceeds one node") != tc.pod {
				t.Fatalf("unexpected configured pool warning: %s", warnings[0])
			}
		})
	}
}

func TestPlanCapacityLookupWarnsWithoutFailing(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		err                error
	}{
		{"valid", "", "", nil},
		{"lookup failed", "", "VM SKU lookup failed", errors.New("private diagnostic")},
		{"missing SKU", `{"value":[]}`, "VM SKU capabilities or reservation model unavailable", nil},
		{"missing memory", `{"value":[{"name":"Standard_D4s_v6","family":"D","capabilities":[{"name":"vCPUs","value":"4"}]}]}`, "VM SKU capabilities or reservation model unavailable", nil},
		{"malformed", `{"value":`, "VM SKU lookup failed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			if tc.name == "valid" {
				tc.output = skuReply(c.AKS.SystemPool.VMSize).output
			}
			r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
				reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`},
				reply{contains: "/providers/Microsoft.Compute/skus?", output: tc.output, err: tc.err})
			var warnings []string
			err := (App{Config: c, Runner: r, Warning: func(s string) { warnings = append(warnings, s) }}).Provision(t.Context(), true)
			if err != nil {
				t.Fatalf("capacity warning failed plan: %v", err)
			}
			if tc.want == "" && len(warnings) != 0 || tc.want != "" && (len(warnings) != 1 ||
				!strings.Contains(warnings[0], "WARNING CONTROL_PLANE_CAPACITY_UNKNOWN: "+tc.want+"; capacity not verified")) {
				t.Fatalf("warnings = %v; want %q", warnings, tc.want)
			}
			if strings.Contains(strings.Join(warnings, ""), "private diagnostic") {
				t.Fatal("warning exposed a raw lookup failure")
			}
			last := strings.Join(r.calls[len(r.calls)-1].Args, " ")
			if !strings.Contains(last, c.Azure.SubscriptionID) || !strings.Contains(last, "eastus2") {
				t.Fatalf("lookup lost its subscription or region: %s", last)
			}
		})
	}
}

type cancelLookupRunner struct {
	cancel context.CancelFunc
}

func (r cancelLookupRunner) Run(_ context.Context, _ Command) ([]byte, error) {
	r.cancel()
	return nil, context.Canceled
}

func TestPlanCapacityPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var warnings []string
	err := (App{Config: testConfig(t), Runner: cancelLookupRunner{cancel}, Warning: func(s string) { warnings = append(warnings, s) }}).warnControlPlaneCapacity(ctx)
	if !errors.Is(err, context.Canceled) || len(warnings) != 0 {
		t.Fatalf("cancellation became a warning: %v, %v", err, warnings)
	}
}

type pagedSKURunner struct {
	calls int
}

func (r *pagedSKURunner) Run(_ context.Context, _ Command) ([]byte, error) {
	r.calls++
	subscription := "22222222-2222-4222-8222-222222222222"
	next := fmt.Sprintf("https://management.azure.com/subscriptions/%s/providers/Microsoft.Compute/skus?page=%d", subscription, r.calls)
	return []byte(fmt.Sprintf(`{"value":[],"nextLink":%q}`, next)), nil
}

func TestPlanCapacityBoundsCatalogPagination(t *testing.T) {
	r := &pagedSKURunner{}
	var warnings []string
	err := (App{Config: testConfig(t), Runner: r, Warning: func(s string) { warnings = append(warnings, s) }}).warnControlPlaneCapacity(t.Context())
	if err != nil || r.calls != 32 || len(warnings) != 1 || !strings.Contains(warnings[0], "CONTROL_PLANE_CAPACITY_UNKNOWN") {
		t.Fatalf("unbounded SKU lookup: calls=%d warnings=%v err=%v", r.calls, warnings, err)
	}
}

type deadlineSKURunner struct {
	t     *testing.T
	calls int
}

func (r *deadlineSKURunner) Run(ctx context.Context, cmd Command) ([]byte, error) {
	r.t.Helper()
	r.calls++
	args := strings.Join(cmd.Args, " ")
	if !strings.HasPrefix(args, "az rest --method get ") ||
		!strings.Contains(args, "/providers/Microsoft.Compute/skus?") {
		r.t.Fatalf("capacity lookup was not a read-only SKU GET: %s", args)
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 {
		r.t.Fatalf("capacity lookup lacks a future deadline: %v, %v", deadline, ok)
	}
	return nil, context.DeadlineExceeded
}

func TestPlanCapacityLookupIsReadOnlyAndBounded(t *testing.T) {
	r := &deadlineSKURunner{t: t}
	var warnings []string
	err := (App{Config: testConfig(t), Runner: r, Warning: func(s string) {
		warnings = append(warnings, s)
	}}).warnControlPlaneCapacity(t.Context())
	if err != nil || r.calls != 1 || len(warnings) != 1 ||
		!strings.HasPrefix(warnings[0], "WARNING CONTROL_PLANE_CAPACITY_UNKNOWN: ") ||
		!strings.HasSuffix(warnings[0], "; capacity not verified") {
		t.Fatalf("timeout must warn without failing: calls=%d warnings=%v err=%v", r.calls, warnings, err)
	}
}
