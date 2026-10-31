// Package discovery resolves gRPC service addresses through Blade's service
// registry (DyServiceDiscoveryService), so the cloud never hardcodes where
// Stargate lives. It mirrors the resolver DysonFS uses for the same fleet.
package discovery

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	gen "src.solsynth.dev/sosys/go/proto"
)

// ResolverScheme routes a logical service name through Blade's registry.
const ResolverScheme = "blade"

const (
	resolveTimeout  = 2 * time.Second
	resolveCacheTTL = time.Second
)

// Resolution is one registry answer. Registered reports whether the service
// has any registration, even if no instance is healthy.
type Resolution struct {
	Endpoints  []string
	Registered bool
}

// Resolver caches registry answers and hands endpoints to the gRPC resolver.
type Resolver struct {
	client gen.DyServiceDiscoveryServiceClient
	conn   *grpc.ClientConn

	mu    sync.Mutex
	cache map[string]cachedResolution
}

type cachedResolution struct {
	resolution Resolution
	expiresAt  time.Time
}

// NewResolver dials Blade's discovery RPC at target.
func NewResolver(target string) (*Resolver, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("service discovery: target is empty")
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("service discovery: dial %s: %w", target, err)
	}
	return &Resolver{
		client: gen.NewDyServiceDiscoveryServiceClient(conn),
		conn:   conn,
		cache:  make(map[string]cachedResolution),
	}, nil
}

// Close closes the discovery connection.
func (r *Resolver) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

// Target returns the gRPC target for a logical service name.
func Target(service string) string {
	return ResolverScheme + ":///" + strings.TrimSpace(service)
}

// Resolve returns healthy gRPC endpoints for a service.
func (r *Resolver) Resolve(ctx context.Context, service string) (Resolution, error) {
	service = normalizeService(service)
	if service == "" {
		return Resolution{}, fmt.Errorf("service discovery: service is required")
	}
	if cached, ok := r.cached(service); ok {
		return cached, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	response, err := r.client.Resolve(callCtx, &gen.DyResolveServiceRequest{Service: service})

	resolution := Resolution{Registered: len(response.GetInstances()) > 0}
	for _, instance := range response.GetInstances() {
		if !instance.GetHealthy() {
			continue
		}
		if value := endpoint(instance); value != "" {
			resolution.Endpoints = append(resolution.Endpoints, value)
		}
	}
	resolution.Endpoints = dedupeEndpoints(resolution.Endpoints)
	r.store(service, resolution)
	return resolution, err
}

func (r *Resolver) cached(service string) (Resolution, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[service]
	if !ok || time.Now().After(entry.expiresAt) {
		return Resolution{}, false
	}
	return entry.resolution, true
}

func (r *Resolver) store(service string, resolution Resolution) {
	r.mu.Lock()
	r.cache[service] = cachedResolution{resolution: resolution, expiresAt: time.Now().Add(resolveCacheTTL)}
	r.mu.Unlock()
}

// endpoint prefers the protocol-keyed map and falls back to the legacy
// grpc_endpoint field.
func endpoint(instance *gen.DyServiceInstance) string {
	if instance == nil {
		return ""
	}
	for name, value := range instance.GetEndpoints() {
		if strings.EqualFold(strings.TrimSpace(name), "grpc") {
			return strings.TrimSpace(value)
		}
	}
	return strings.TrimSpace(instance.GetGrpcEndpoint())
}

func normalizeService(service string) string {
	return strings.ToLower(strings.TrimSpace(service))
}

// endpointAddress strips any scheme from a registered endpoint.
func endpointAddress(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	return parsed.Host
}

func dedupeEndpoints(endpoints []string) []string {
	seen := make(map[string]struct{}, len(endpoints))
	out := make([]string, 0, len(endpoints))
	for _, value := range endpoints {
		value = endpointAddress(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
