// Copyright 2026 The frp Authors
//
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

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/util/xlog"
)

const (
	sourceResolveTimeout = 5 * time.Second
	sourceResolveTTL     = 30 * time.Second
)

// hostPort is a resolved backend address.
type hostPort struct {
	host string
	port int
}

func (h hostPort) address() string {
	return net.JoinHostPort(h.host, strconv.Itoa(h.port))
}

// sourceResolver resolves a "host[:port]" value from a v1.ValueSource (DNS TXT
// record, file or command) and caches the result for a short while. Lookup
// failures fall back to the last known value, and to the static configuration
// when nothing has been resolved yet.
type sourceResolver struct {
	source  *v1.ValueSource
	ttl     time.Duration
	timeout time.Duration
	// lookup fetches the raw value; tests replace it.
	lookup func(context.Context) (string, error)

	mu     sync.Mutex
	cached *hostPort
	at     time.Time
}

func newSourceResolver(source *v1.ValueSource) *sourceResolver {
	if source == nil {
		return nil
	}
	return &sourceResolver{
		source:  source,
		ttl:     sourceResolveTTL,
		timeout: sourceResolveTimeout,
		lookup:  source.Resolve,
	}
}

// resolve returns the configured backend, resolving it again when the cached
// value expired.
func (r *sourceResolver) resolve(ctx context.Context, fallbackHost string, fallbackPort int) (hostPort, error) {
	fallback := hostPort{host: fallbackHost, port: fallbackPort}
	xl := xlog.FromContextSafe(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Since(r.at) < r.ttl {
		return *r.cached, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := r.lookup(lookupCtx)
	if err != nil {
		if r.cached != nil {
			xl.Warnf("failed to refresh address from %s, reuse %s: %v",
				describeSource(r.source), r.cached.address(), err)
			return *r.cached, nil
		}
		xl.Warnf("failed to resolve address from %s, fall back to %s: %v",
			describeSource(r.source), fallback.address(), err)
		return fallback, nil
	}

	resolved, err := parseHostPort(raw, fallbackHost, fallbackPort)
	if err != nil {
		if r.cached != nil {
			xl.Warnf("invalid address %q from %s, reuse %s: %v",
				raw, describeSource(r.source), r.cached.address(), err)
			return *r.cached, nil
		}
		xl.Warnf("invalid address %q from %s, fall back to %s: %v",
			raw, describeSource(r.source), fallback.address(), err)
		return fallback, nil
	}

	if r.cached == nil || *r.cached != resolved {
		xl.Infof("resolved %s -> %s", describeSource(r.source), resolved.address())
	}
	r.cached = &resolved
	r.at = time.Now()
	return resolved, nil
}

// resolvePort resolves a bare port (used for the remote port requested from
// frps). The host part of the resolved value is ignored.
func (r *sourceResolver) resolvePort(ctx context.Context, fallbackPort int) (int, error) {
	resolved, err := r.resolve(ctx, "", fallbackPort)
	if err != nil {
		return 0, err
	}
	if resolved.port <= 0 {
		return 0, errors.New("no port in resolved value")
	}
	return resolved.port, nil
}

// parseHostPort parses "host", "host:port", "[ipv6]" or "[ipv6]:port". A missing
// host keeps fallbackHost, a missing port keeps fallbackPort.
func parseHostPort(raw string, fallbackHost string, fallbackPort int) (hostPort, error) {
	value := firstAddressToken(raw)
	if value == "" {
		return hostPort{}, errors.New("resolved value is empty")
	}

	host, portText := splitHostPortText(value)
	if host == "" {
		// A value like ":7000" only carries a port; callers that need a host
		// (the dial target) apply their own default, resolvePort ignores it.
		host = fallbackHost
	}

	resolved := hostPort{host: host, port: fallbackPort}
	if portText == "" {
		return resolved, nil
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return hostPort{}, fmt.Errorf("invalid port %q", portText)
	}
	resolved.port = port
	return resolved, nil
}

func firstAddressToken(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if token := strings.Trim(fields[0], "\"'"); token != "" {
			return token
		}
	}
	return ""
}

func splitHostPortText(value string) (string, string) {
	if strings.HasPrefix(value, "[") {
		if host, port, err := net.SplitHostPort(value); err == nil {
			return host, port
		}
		return strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"), ""
	}
	switch strings.Count(value, ":") {
	case 0:
		return value, ""
	case 1:
		if host, port, err := net.SplitHostPort(value); err == nil {
			return host, port
		}
		parts := strings.SplitN(value, ":", 2)
		return parts[0], parts[1]
	default:
		return value, "" // bare IPv6
	}
}

func describeSource(source *v1.ValueSource) string {
	if source == nil {
		return "configuration"
	}
	switch source.Type {
	case "txt":
		if source.TXT != nil {
			return fmt.Sprintf("TXT record %s", source.TXT.Record)
		}
	case "file":
		if source.File != nil {
			return fmt.Sprintf("file %s", source.File.Path)
		}
	case "exec":
		if source.Exec != nil {
			return fmt.Sprintf("command %s", source.Exec.Command)
		}
	}
	return "value source"
}

// remotePortSourceOf returns the dynamic remote port source of a proxy config,
// together with the statically configured port.
func remotePortSourceOf(cfg v1.ProxyConfigurer) (*v1.ValueSource, int) {
	switch c := cfg.(type) {
	case *v1.TCPProxyConfig:
		return c.RemotePortSource, c.RemotePort
	case *v1.UDPProxyConfig:
		return c.RemotePortSource, c.RemotePort
	default:
		return nil, 0
	}
}
