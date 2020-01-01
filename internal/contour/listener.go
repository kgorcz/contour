// Copyright © 2018 Heptio
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package contour

import (
	"strconv"
	"sync"

	"github.com/sirupsen/logrus"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	"github.com/gogo/protobuf/proto"
	"github.com/heptio/contour/internal/dag"
	"github.com/heptio/contour/internal/envoy"
	v1 "k8s.io/api/core/v1"
)

const (
	ENVOY_HTTP_LISTENER            = "ingress_http"
	ENVOY_HTTPS_LISTENER           = "ingress_https"
	DEFAULT_HTTP_ACCESS_LOG        = "/dev/stdout"
	DEFAULT_HTTP_LISTENER_ADDRESS  = "0.0.0.0"
	DEFAULT_HTTP_LISTENER_PORT     = 8080
	DEFAULT_HTTPS_ACCESS_LOG       = "/dev/stdout"
	DEFAULT_HTTPS_LISTENER_ADDRESS = DEFAULT_HTTP_LISTENER_ADDRESS
	DEFAULT_HTTPS_LISTENER_PORT    = 8443
)

// ListenerVisitorConfig holds configuration parameters for visitListeners.
type ListenerVisitorConfig struct {
	// Envoy's HTTP (non TLS) listener address.
	// If not set, defaults to DEFAULT_HTTP_LISTENER_ADDRESS.
	HTTPAddress string

	// Envoy's HTTP (non TLS) listener port.
	// If not set, defaults to DEFAULT_HTTP_LISTENER_PORT.
	HTTPPort int

	// Envoy's HTTP (non TLS) access log path.
	// If not set, defaults to DEFAULT_HTTP_ACCESS_LOG.
	HTTPAccessLog string

	// Envoy's HTTPS (TLS) listener address.
	// If not set, defaults to DEFAULT_HTTPS_LISTENER_ADDRESS.
	HTTPSAddress string

	// Envoy's HTTPS (TLS) listener port.
	// If not set, defaults to DEFAULT_HTTPS_LISTENER_PORT.
	HTTPSPort int

	// Envoy's HTTPS (TLS) access log path.
	// If not set, defaults to DEFAULT_HTTPS_ACCESS_LOG.
	HTTPSAccessLog string

	// UseProxyProto configurs all listeners to expect a PROXY protocol
	// V1 header on new connections.
	// If not set, defaults to false.
	UseProxyProto bool
}

// httpAddress returns the port for the HTTP (non TLS)
// listener or DEFAULT_HTTP_LISTENER_ADDRESS if not configured.
func (lvc *ListenerVisitorConfig) httpAddress() string {
	if lvc.HTTPAddress != "" {
		return lvc.HTTPAddress
	}
	return DEFAULT_HTTP_LISTENER_ADDRESS
}

// httpPort returns the port for the HTTP (non TLS)
// listener or DEFAULT_HTTP_LISTENER_PORT if not configured.
func (lvc *ListenerVisitorConfig) httpPort() int {
	if lvc.HTTPPort != 0 {
		return lvc.HTTPPort
	}
	return DEFAULT_HTTP_LISTENER_PORT
}

// httpAccessLog returns the access log for the HTTP (non TLS)
// listener or DEFAULT_HTTP_ACCESS_LOG if not configured.
func (lvc *ListenerVisitorConfig) httpAccessLog() string {
	if lvc.HTTPAccessLog != "" {
		return lvc.HTTPAccessLog
	}
	return DEFAULT_HTTP_ACCESS_LOG
}

// httpsAddress returns the port for the HTTPS (TLS)
// listener or DEFAULT_HTTPS_LISTENER_ADDRESS if not configured.
func (lvc *ListenerVisitorConfig) httpsAddress() string {
	if lvc.HTTPSAddress != "" {
		return lvc.HTTPSAddress
	}
	return DEFAULT_HTTPS_LISTENER_ADDRESS
}

// httpsPort returns the port for the HTTPS (TLS) listener
// or DEFAULT_HTTPS_LISTENER_PORT if not configured.
func (lvc *ListenerVisitorConfig) httpsPort() int {
	if lvc.HTTPSPort != 0 {
		return lvc.HTTPSPort
	}
	return DEFAULT_HTTPS_LISTENER_PORT
}

// httpsAccessLog returns the access log for the HTTPS (TLS)
// listener or DEFAULT_HTTPS_ACCESS_LOG if not configured.
func (lvc *ListenerVisitorConfig) httpsAccessLog() string {
	if lvc.HTTPSAccessLog != "" {
		return lvc.HTTPSAccessLog
	}
	return DEFAULT_HTTPS_ACCESS_LOG
}

// ListenerCache manages the contents of the gRPC LDS cache.
type ListenerCache struct {
	mu      sync.Mutex
	values  map[string]*v2.Listener
	waiters []chan int
	last    int
}

// Register registers ch to receive a value when Notify is called.
// The value of last is the count of the times Notify has been called on this Cache.
// It functions of a sequence counter, if the value of last supplied to Register
// is less than the Cache's internal counter, then the caller has missed at least
// one notification and will fire immediately.
//
// Sends by the broadcaster to ch must not block, therefor ch must have a capacity
// of at least 1.
func (c *ListenerCache) Register(ch chan int, last int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if last < c.last {
		// notify this channel immediately
		ch <- c.last
		return
	}
	c.waiters = append(c.waiters, ch)
}

// Update replaces the contents of the cache with the supplied map.
func (c *ListenerCache) Update(v map[string]*v2.Listener) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.values = v
	c.notify()
}

// notify notifies all registered waiters that an event has occurred.
func (c *ListenerCache) notify() {
	c.last++

	for _, ch := range c.waiters {
		ch <- c.last
	}
	c.waiters = c.waiters[:0]
}

// Values returns a slice of the value stored in the cache.
func (c *ListenerCache) Values(filter func(string) bool) []proto.Message {
	c.mu.Lock()
	values := make([]proto.Message, 0, len(c.values))
	for _, v := range c.values {
		if filter(v.Name) {
			values = append(values, v)
		}
	}
	c.mu.Unlock()
	return values
}

type listenerVisitor struct {
	*ListenerVisitorConfig

	listeners map[string]*v2.Listener
	http      bool // at least one dag.VirtualHost encountered
}

func visitListeners(root dag.Vertex, lvc *ListenerVisitorConfig) map[string]*v2.Listener {
	lv := listenerVisitor{
		ListenerVisitorConfig: lvc,
		listeners: map[string]*v2.Listener{
			ENVOY_HTTP_LISTENER: {
				Name:    ENVOY_HTTP_LISTENER,
				Address: envoy.SocketAddress(lvc.httpAddress(), lvc.httpPort()),
				FilterChains: []listener.FilterChain{{
					Filters: []listener.Filter{
						envoy.HTTPConnectionManager(ENVOY_HTTP_LISTENER, lvc.httpAccessLog()),
					},
					UseProxyProto: bv(lvc.UseProxyProto),
				}},
			},
			ENVOY_HTTPS_LISTENER: {
				Name:    ENVOY_HTTPS_LISTENER,
				Address: envoy.SocketAddress(lvc.httpsAddress(), lvc.httpsPort()),
				ListenerFilters: []listener.ListenerFilter{
					envoy.TLSInspector(),
				},
			},
		},
	}
	lv.visit(root)

	if !lv.http {
		delete(lv.listeners, ENVOY_HTTP_LISTENER)
	}
	if len(lv.listeners[ENVOY_HTTPS_LISTENER].FilterChains) == 0 {
		delete(lv.listeners, ENVOY_HTTPS_LISTENER)
	}
	return lv.listeners
}

func (v *listenerVisitor) visit(vertex dag.Vertex) {
	switch vh := vertex.(type) {
	case *dag.VirtualHost:
		// we only create on http listener so record the fact
		// that we need to then double back at the end and add
		// the listener properly.
		v.http = true
	case *dag.SecureVirtualHost:
		data := vh.Data()
		if data == nil || len(data[v1.TLSCertKey]) == 0 || len(data[v1.TLSPrivateKeyKey]) == 0 {
			// no secret for this vhost, skip it
			logrus.Info("Skipping secure virtual host due to missing/empty secret: ", vh.VirtualHost.Host)
			return
		}
		filters := []listener.Filter{
			envoy.HTTPConnectionManager(ENVOY_HTTPS_LISTENER, v.httpsAccessLog()),
		}
		alpnProtos := []string{"h2", "http/1.1"}
		if vh.VirtualHost.TCPProxy != nil {
			filters = []listener.Filter{
				envoy.TCPProxy(ENVOY_HTTPS_LISTENER, vh.VirtualHost.TCPProxy, v.httpsAccessLog()),
			}
			alpnProtos = nil // do not offer ALPN
		}

		if vh.VirtualHost.Port != 443 {
			logrus.Info("Creating tcp virtual host: ", vh.VirtualHost.Host, " | ", vh.VirtualHost.Port)
			name := "ingress_tcp_port_" + strconv.Itoa(vh.VirtualHost.Port)
			v.listeners[name] = &v2.Listener{
				Name:    name,
				Address: envoy.SocketAddress("0.0.0.0", vh.VirtualHost.Port),
				FilterChains: []listener.FilterChain{
					listener.FilterChain{
						TlsContext: envoy.DownstreamTLSContext(data[v1.TLSCertKey], data[v1.TLSPrivateKeyKey], vh.MinProtoVersion),
						Filters: []listener.Filter{
							envoy.TCPProxyFilter(name, vh.TCPProxy, v.httpsAccessLog()),
						},
					},
				},
			}
			alpnProtos = nil // do not offer ALPN
			return
		}

		logrus.Info("Creating https virtual host: ", vh.VirtualHost.Host, " | ", vh.VirtualHost.Port)

		// new
		fc := listener.FilterChain{
			FilterChainMatch: &listener.FilterChainMatch{
				ServerNames: []string{vh.Host},
			},
			TlsContext:    envoy.DownstreamTLSContext(data[v1.TLSCertKey], data[v1.TLSPrivateKeyKey], vh.MinProtoVersion, alpnProtos...),
			Filters:       filters,
			UseProxyProto: bv(v.UseProxyProto),
		}
		v.listeners[ENVOY_HTTPS_LISTENER].FilterChains = append(v.listeners[ENVOY_HTTPS_LISTENER].FilterChains, fc)
	default:
		// recurse
		vertex.Visit(v.visit)
	}
}
