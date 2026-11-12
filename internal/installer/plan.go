package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"regexp"
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
	if s.Sizing != nil {
		if a := s.Sizing.APIServer; a != nil {
			var err error
			api, err = containerRequests(a.Resources)
			if err != nil {
				return api, router, controlPlaneRequest{}, err
			}
			if a.Replicas != nil {
				apiReplicas = *a.Replicas
			}
		}
		if r := s.Sizing.Router; r != nil {
			if r.Replicas != nil {
				routerReplicas = *r.Replicas
			}
			if r.Resources != nil {
				for _, resources := range []*ResourceRequirements{r.Resources.AtenetRouter, r.Resources.Envoy} {
					part, err := containerRequests(resources)
					if err != nil {
						return api, router, controlPlaneRequest{}, err
					}
					router.add(part)
				}
			}
		}
	}
	total := api.scaled(apiReplicas)
	total.add(router.scaled(routerReplicas))
	return api, router, total, nil
}

var memoryGBPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

func (a App) systemCapacity(data []byte, size string) (resource.Quantity, *big.Rat, error) {
	_, cpus, err := a.cloudSKU(data, size)
	if err != nil {
		return resource.Quantity{}, nil, err
	}
	// AKS Linux reservations: https://learn.microsoft.com/azure/aks/node-resource-reservations
	reservations := map[int64]int64{1: 60, 2: 100, 4: 140, 8: 180, 16: 260, 32: 420, 64: 740}
	reserved, ok := reservations[cpus]
	if !ok {
		return resource.Quantity{}, nil, fmt.Errorf("unsupported CPU reservation profile")
	}
	var skus []struct {
		Name         string
		Capabilities []struct{ Name, Value string }
	}
	if err := json.Unmarshal(data, &skus); err != nil {
		return resource.Quantity{}, nil, err
	}
	matches, memoryFields, cpuFields := 0, 0, 0
	var memory string
	for _, sku := range skus {
		if !strings.EqualFold(sku.Name, size) {
			continue
		}
		matches++
		for _, capability := range sku.Capabilities {
			if capability.Name == "vCPUs" {
				cpuFields++
			}
			if capability.Name == "MemoryGB" {
				memoryFields++
				memory = capability.Value
			}
		}
	}
	if matches != 1 || cpuFields != 1 || memoryFields != 1 || !memoryGBPattern.MatchString(memory) || len(memory) > 20 {
		return resource.Quantity{}, nil, fmt.Errorf("missing or invalid memory capability")
	}
	mem, ok := new(big.Rat).SetString(memory)
	if !ok || mem.Sign() <= 0 || mem.Cmp(big.NewRat(65536, 1)) > 0 {
		return resource.Quantity{}, nil, fmt.Errorf("missing or invalid memory capability")
	}
	mem.Mul(mem, big.NewRat(3*(1<<30), 4))
	mem.Sub(mem, big.NewRat(100*(1<<20), 1))
	if mem.Sign() <= 0 {
		return resource.Quantity{}, nil, fmt.Errorf("insufficient memory for the reservation model")
	}
	return *resource.NewMilliQuantity(cpus*1000-reserved, resource.DecimalSI), mem, nil
}

func quantityRat(q resource.Quantity) *big.Rat {
	value, _ := new(big.Rat).SetString(q.AsDec().String())
	return value
}

func capacityWarning(size string, count int, cpu resource.Quantity, memory *big.Rat, s SubstrateConfig) (string, error) {
	api, router, total, err := controlPlaneRequests(s)
	if err != nil {
		return "", err
	}
	totalCPU := cpu.DeepCopy()
	totalCPU.Mul(int64(count))
	totalMemory := new(big.Rat).Mul(memory, big.NewRat(int64(count), 1))
	var reasons []string
	if total.cpu.Cmp(totalCPU) > 0 || quantityRat(total.memory).Cmp(totalMemory) > 0 {
		reasons = append(reasons, "aggregate requests exceed the pool estimate")
	}
	if api.cpu.Cmp(cpu) > 0 || router.cpu.Cmp(cpu) > 0 ||
		quantityRat(api.memory).Cmp(memory) > 0 || quantityRat(router.memory).Cmp(memory) > 0 {
		reasons = append(reasons, "a pod exceeds one node's estimate")
	}
	if len(reasons) == 0 {
		return "", nil
	}
	// Flooring the display avoids overstating allocatable memory.
	bytes := new(big.Int).Quo(totalMemory.Num(), totalMemory.Denom())
	memDisplay, err := resource.ParseQuantity(bytes.String())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("WARNING CONTROL_PLANE_CAPACITY: system=%sx%d requested=<%s,%s> estimated-allocatable=<%s,%s>; %s; estimate excludes system/add-on pods; no fit guarantee",
		size, count, total.cpu.String(), total.memory.String(), totalCPU.String(), memDisplay.String(), strings.Join(reasons, ", ")), nil
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
	// The system pool is fixed in the current infrastructure template.
	size, count := SystemVMSize, 1
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
