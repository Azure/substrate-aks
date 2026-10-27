package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

const unrestrictedAPIWarning = "AKS API is unrestricted and internet-reachable; Kubernetes authentication is still required."

func resolveAPICIDR(ctx context.Context, cidr string, unrestricted bool, client *http.Client) (string, error) {
	if unrestricted {
		if cidr != "" {
			return "", errors.New("--api-cidr and --allow-unrestricted-api are mutually exclusive")
		}
		return "", nil
	}
	if cidr != "" {
		return cidr, nil
	}
	if client == nil {
		client = &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("detecting public IPv4 via api.ipify.org; supply --api-cidr explicitly: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("public IPv4 lookup returned HTTP %d; supply --api-cidr explicitly", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 65))
	if err != nil {
		return "", fmt.Errorf("reading public IPv4; supply --api-cidr explicitly: %w", err)
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(data)))
	if len(data) > 64 || err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", errors.New("public IPv4 lookup did not return a public IPv4 address; supply --api-cidr explicitly")
	}
	return netip.PrefixFrom(ip, 32).String(), nil
}

func printAPIAccess(out io.Writer, c installer.AKSConfig) {
	if c.AllowUnrestrictedAPI {
		fmt.Fprintln(out, "WARNING:", unrestrictedAPIWarning)
	} else {
		fmt.Fprintln(out, "AKS API allowed IPv4 CIDRs:", strings.Join(c.APIAuthorizedCIDRs, ", "))
	}
}
