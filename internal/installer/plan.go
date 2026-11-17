package installer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

type controlPlaneRequest struct {
	cpu, memory resource.Quantity
}

func containerRequests(r *ResourceRequirements) (controlPlaneRequest, error) {
	result := controlPlaneRequest{}
	if r == nil {
		return result, nil
	}
	for _, field := range []struct {
		name string
		dest *resource.Quantity
	}{{"cpu", &result.cpu}, {"memory", &result.memory}} {
		value, ok := r.Requests[field.name]
		if !ok {
			value, ok = r.Limits[field.name]
		}
		if ok {
			q, err := resource.ParseQuantity(value)
			if err != nil || q.Sign() < 0 {
				return result, fmt.Errorf("invalid %s resource quantity", field.name)
			}
			*field.dest = q
		}
	}
	return result, nil
}

func (r *controlPlaneRequest) add(other controlPlaneRequest) {
	r.cpu.Add(other.cpu)
	r.memory.Add(other.memory)
}

func (r controlPlaneRequest) scaled(count int) controlPlaneRequest {
	r.cpu = r.cpu.DeepCopy()
	r.memory = r.memory.DeepCopy()
	r.cpu.Mul(int64(count))
	r.memory.Mul(int64(count))
	return r
}

func controlPlaneRequests(s SubstrateConfig) (controlPlaneRequest, controlPlaneRequest, controlPlaneRequest, error) {
	var api, router controlPlaneRequest
	apiReplicas, routerReplicas := defaultAPIServerReplicas, 1
	for _, workload := range s.sizedWorkloads() {
		var request controlPlaneRequest
		for _, container := range workload.containers {
			part, err := containerRequests(container.resources)
			if err != nil {
				return api, router, controlPlaneRequest{}, err
			}
			request.add(part)
		}
		switch workload.name {
		case "ate-api-server":
			api = request
			if workload.replicas != nil {
				apiReplicas = *workload.replicas
			}
		case "atenet-router":
			router = request
			if workload.replicas != nil {
				routerReplicas = *workload.replicas
			}
		}
	}
	total := api.scaled(apiReplicas)
	total.add(router.scaled(routerReplicas))
	return api, router, total, nil
}

func (a App) systemCapacity(data []byte, size string) (resource.Quantity, resource.Quantity, error) {
	sku, err := a.readSKU(data, size)
	if err != nil {
		return resource.Quantity{}, resource.Quantity{}, err
	}
	// AKS Linux reservations: https://learn.microsoft.com/azure/aks/node-resource-reservations
	reservations := map[int64]int64{1: 60, 2: 100, 4: 140, 8: 180, 16: 260, 32: 420, 64: 740}
	reserved, ok := reservations[sku.cpus]
	if !ok {
		return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("unsupported CPU reservation profile")
	}
	memory := sku.memory
	if sku.matches != 1 || sku.cpusFound != 1 || sku.memoryFound != 1 ||
		len(memory) == 0 || len(memory) > 20 || strings.Trim(memory, "0123456789.") != "" ||
		strings.HasPrefix(memory, ".") || strings.HasSuffix(memory, ".") {
		return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("missing or invalid memory capability")
	}
	var q resource.Quantity
	if _, ok := q.AsDec().SetString(memory); !ok || q.Sign() <= 0 || q.CmpInt64(65536) > 0 {
		return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("missing or invalid memory capability")
	}
	q.Mul(3 * (1 << 28))
	q.Sub(*resource.NewQuantity(100*(1<<20), resource.DecimalSI))
	if q.Sign() <= 0 {
		return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("insufficient memory for the reservation model")
	}
	return *resource.NewMilliQuantity(sku.cpus*1000-reserved, resource.DecimalSI), q, nil
}

func capacityWarning(size string, count int, cpu, memory resource.Quantity, s SubstrateConfig) (string, error) {
	api, router, total, err := controlPlaneRequests(s)
	if err != nil {
		return "", err
	}
	totalCPU := cpu.DeepCopy()
	totalCPU.Mul(int64(count))
	totalMemory := memory.DeepCopy()
	totalMemory.Mul(int64(count))
	var reasons []string
	if total.cpu.Cmp(totalCPU) > 0 || total.memory.Cmp(totalMemory) > 0 {
		reasons = append(reasons, "aggregate requests exceed the pool estimate")
	}
	if api.cpu.Cmp(cpu) > 0 || router.cpu.Cmp(cpu) > 0 ||
		api.memory.Cmp(memory) > 0 || router.memory.Cmp(memory) > 0 {
		reasons = append(reasons, "a pod exceeds one node's estimate")
	}
	if len(reasons) == 0 {
		return "", nil
	}
	displayMemory := totalMemory.DeepCopy()
	displayMemory.Format = resource.DecimalSI
	return fmt.Sprintf("WARNING CONTROL_PLANE_CAPACITY: system=%sx%d requested=<%s,%s> estimated-allocatable=<%s,%s>; %s; estimate excludes system/add-on pods; no fit guarantee",
		size, count, total.cpu.String(), total.memory.String(), totalCPU.String(), displayMemory.String(), strings.Join(reasons, ", ")), nil
}

func (a App) warnControlPlaneCapacity(ctx context.Context) error {
	emit := func(message string) {
		if a.Warning != nil {
			a.Warning(message)
		} else {
			fmt.Fprintln(os.Stderr, message)
		}
	}
	lookup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, err := a.regionalSKUCatalogPages(lookup, 32)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		emit("WARNING CONTROL_PLANE_CAPACITY_UNKNOWN: VM SKU lookup failed; capacity not verified")
		return nil
	}
	if a.Config.Substrate.Source.Commit != DefaultCommit {
		emit("WARNING CONTROL_PLANE_CAPACITY_UNKNOWN: control-plane defaults for this source revision are not verified; capacity not verified")
		return nil
	}
	size, count := a.Config.AKS.SystemPool.VMSize, a.Config.AKS.SystemPool.Count
	cpu, memory, err := a.systemCapacity(data, size)
	if err != nil {
		emit("WARNING CONTROL_PLANE_CAPACITY_UNKNOWN: VM SKU capabilities or reservation model unavailable; capacity not verified")
		return nil
	}
	warning, err := capacityWarning(size, count, cpu, memory, a.Config.Substrate)
	if err != nil {
		emit("WARNING CONTROL_PLANE_CAPACITY_UNKNOWN: requested resources could not be estimated; capacity not verified")
		return nil
	}
	if warning != "" {
		emit(warning)
	}
	return nil
}
