package discovery

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	gen "src.solsynth.dev/sosys/go/proto"
)

type fakeDiscoveryClient struct {
	gen.DyServiceDiscoveryServiceClient
	response *gen.DyResolveServiceResponse
	err      error
}

func (f *fakeDiscoveryClient) Resolve(context.Context, *gen.DyResolveServiceRequest, ...grpc.CallOption) (*gen.DyResolveServiceResponse, error) {
	return f.response, f.err
}

func TestResolverUsesHealthyRegisteredGRPCEndpoint(t *testing.T) {
	resolver := &Resolver{
		client: &fakeDiscoveryClient{response: &gen.DyResolveServiceResponse{Instances: []*gen.DyServiceInstance{{
			Service: "stargate", InstanceId: "stargate-1", Healthy: true, GrpcEndpoint: "stargate-1:9090",
		}}}},
		cache: make(map[string]cachedResolution),
	}

	resolution, err := resolver.Resolve(context.Background(), "stargate")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolution.Endpoints) != 1 || resolution.Endpoints[0] != "stargate-1:9090" {
		t.Fatalf("endpoints = %v, want the registered endpoint", resolution.Endpoints)
	}
}

func TestResolverPrefersProtocolKeyedEndpoint(t *testing.T) {
	resolver := &Resolver{
		client: &fakeDiscoveryClient{response: &gen.DyResolveServiceResponse{Instances: []*gen.DyServiceInstance{{
			Service:      "stargate",
			InstanceId:   "stargate-1",
			Healthy:      true,
			GrpcEndpoint: "legacy:9090",
			Endpoints:    map[string]string{"grpc": "grpcs://stargate-1:9091"},
		}}}},
		cache: make(map[string]cachedResolution),
	}

	resolution, err := resolver.Resolve(context.Background(), "stargate")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolution.Endpoints) != 1 || resolution.Endpoints[0] != "stargate-1:9091" {
		t.Fatalf("endpoints = %v, want the protocol-keyed endpoint without scheme", resolution.Endpoints)
	}
}

func TestResolverSkipsUnhealthyInstances(t *testing.T) {
	resolver := &Resolver{
		client: &fakeDiscoveryClient{response: &gen.DyResolveServiceResponse{Instances: []*gen.DyServiceInstance{
			{Service: "stargate", InstanceId: "stargate-1", Healthy: false, GrpcEndpoint: "stargate-1:9090"},
			{Service: "stargate", InstanceId: "stargate-2", Healthy: true, GrpcEndpoint: "stargate-2:9090"},
		}}},
		cache: make(map[string]cachedResolution),
	}

	resolution, _ := resolver.Resolve(context.Background(), "stargate")
	if len(resolution.Endpoints) != 1 || resolution.Endpoints[0] != "stargate-2:9090" {
		t.Fatalf("endpoints = %v, want only the healthy instance", resolution.Endpoints)
	}
}

func TestResolverReportsEmptyWhenRegisteredButUnhealthy(t *testing.T) {
	resolver := &Resolver{
		client: &fakeDiscoveryClient{response: &gen.DyResolveServiceResponse{Instances: []*gen.DyServiceInstance{{
			Service: "stargate", InstanceId: "stargate-1", Healthy: false,
		}}}},
		cache: make(map[string]cachedResolution),
	}

	resolution, err := resolver.Resolve(context.Background(), "stargate")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolution.Endpoints) != 0 || !resolution.Registered {
		t.Fatalf("resolution = %+v, want registered with no endpoints", resolution)
	}
}

func TestResolverSurfacesRPCError(t *testing.T) {
	failure := errors.New("registry unavailable")
	resolver := &Resolver{
		client: &fakeDiscoveryClient{err: failure},
		cache:  make(map[string]cachedResolution),
	}

	if _, err := resolver.Resolve(context.Background(), "stargate"); !errors.Is(err, failure) {
		t.Fatalf("Resolve() error = %v, want %v", err, failure)
	}
}

func TestTarget(t *testing.T) {
	if got := Target("stargate"); got != "blade:///stargate" {
		t.Fatalf("Target() = %q", got)
	}
}
