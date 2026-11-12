package installer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestControlPlaneCapacityWarnings(t *testing.T) {
	cpu := resource.MustParse("3860m")
	memory := quantityRat(resource.MustParse("12188Mi"))
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
			got, err := capacityWarning(SystemVMSize, tc.count, cpu, memory, SubstrateConfig{Sizing: &sizing})
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
	memory := quantityRat(resource.MustParse("20Gi"))
	warning, err := capacityWarning("Standard_D8s_v6", 3, cpu, memory, c)
	if err != nil || !strings.Contains(warning, "system=Standard_D8s_v6x3") ||
		!strings.Contains(warning, "a pod exceeds one node") || strings.Contains(warning, "aggregate requests exceed") {
		t.Fatalf("different pool estimate: %s, %v", warning, err)
	}
}

func TestControlPlaneCapacityCombinesRouterContainers(t *testing.T) {
	cpu := resource.MustParse("3860m")
	memory := quantityRat(resource.MustParse("12188Mi"))
	c := SubstrateConfig{Sizing: &SizingConfig{Router: &RouterSizing{Resources: &RouterResources{
		AtenetRouter: &ResourceRequirements{Requests: map[string]string{"cpu": "2"}},
		Envoy:        &ResourceRequirements{Requests: map[string]string{"cpu": "2"}},
	}}}}
	warning, err := capacityWarning(SystemVMSize, 2, cpu, memory, c)
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
		{"excess memory", "4", "99999999", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fmt.Sprintf(`[{"name":%q,"family":"D","capabilities":[{"name":"vCPUs","value":%q},{"name":"MemoryGB","value":%q}]}]`,
				SystemVMSize, tc.cpus, tc.memory)
			cpu, memory, err := a.systemCapacity([]byte(data), SystemVMSize)
			if (err == nil) != tc.valid {
				t.Fatalf("systemCapacity = %v; want valid %t", err, tc.valid)
			}
			if tc.name == "valid" && (cpu.Cmp(resource.MustParse("3860m")) != 0 ||
				memory.Cmp(quantityRat(resource.MustParse("12188Mi"))) != 0) {
				t.Fatalf("wrong reservation: %s, %s", cpu.String(), memory.RatString())
			}
		})
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
				SystemVMSize, fmt.Sprint(tc.cpus))
			cpu, _, err := a.systemCapacity([]byte(data), SystemVMSize)
			want := *resource.NewMilliQuantity(tc.cpus*1000-tc.reserved, resource.DecimalSI)
			if err != nil || cpu.Cmp(want) != 0 {
				t.Fatalf("capacity = %s, %v; want %s", cpu.String(), err, want.String())
			}
		})
	}
}

func TestPlanCapacityLookupWarnsWithoutFailing(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		err                error
	}{
		{"valid", skuReply(SystemVMSize).output, "", nil},
		{"lookup failed", "", "VM SKU lookup failed", errors.New("private diagnostic")},
		{"missing SKU", `{"value":[]}`, "VM SKU capabilities or reservation model unavailable", nil},
		{"missing memory", `{"value":[{"name":"Standard_D4s_v6","family":"D","capabilities":[{"name":"vCPUs","value":"4"}]}]}`, "VM SKU capabilities or reservation model unavailable", nil},
		{"malformed", `{"value":`, "VM SKU lookup failed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
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
