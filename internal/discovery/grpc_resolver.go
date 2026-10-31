package discovery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/resolver"
)

const (
	resolverPollInterval    = 5 * time.Second
	roundRobinServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`
)

var (
	activeResolver       atomic.Pointer[Resolver]
	registerResolverOnce sync.Once
)

// RegisterResolver installs the process-wide blade:/// gRPC resolver. It must
// run before any client dials a blade:/// target.
func RegisterResolver(r *Resolver) {
	if r == nil {
		return
	}
	activeResolver.Store(r)
	registerResolverOnce.Do(func() {
		resolver.Register(&bladeResolverBuilder{})
	})
}

type bladeResolverBuilder struct{}

func (b *bladeResolverBuilder) Scheme() string { return ResolverScheme }

func (b *bladeResolverBuilder) Build(target resolver.Target, cc resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	service := strings.TrimSpace(target.Endpoint())
	if service == "" {
		return nil, fmt.Errorf("blade resolver: service name is required")
	}
	r := &bladeResolver{
		service: service,
		cc:      cc,
		stop:    make(chan struct{}),
		trigger: make(chan struct{}, 1),
	}
	go r.run()
	return r, nil
}

type bladeResolver struct {
	service string
	cc      resolver.ClientConn
	stop    chan struct{}
	trigger chan struct{}
	once    sync.Once
}

func (r *bladeResolver) ResolveNow(resolver.ResolveNowOptions) {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *bladeResolver) Close() {
	r.once.Do(func() { close(r.stop) })
}

func (r *bladeResolver) run() {
	r.refresh()
	ticker := time.NewTicker(resolverPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.refresh()
		case <-r.trigger:
			r.refresh()
		}
	}
}

func (r *bladeResolver) refresh() {
	active := activeResolver.Load()
	if active == nil {
		r.cc.ReportError(fmt.Errorf("blade resolver: service discovery is not configured"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	resolution, err := active.Resolve(ctx, r.service)
	if err != nil && len(resolution.Endpoints) == 0 {
		r.cc.ReportError(err)
		return
	}
	addresses := make([]resolver.Address, 0, len(resolution.Endpoints))
	for _, value := range resolution.Endpoints {
		if address := endpointAddress(value); address != "" {
			addresses = append(addresses, resolver.Address{Addr: address})
		}
	}
	state := resolver.State{Addresses: addresses}
	if len(addresses) > 0 {
		state.ServiceConfig = r.cc.ParseServiceConfig(roundRobinServiceConfig)
	}
	r.cc.UpdateState(state)
}
