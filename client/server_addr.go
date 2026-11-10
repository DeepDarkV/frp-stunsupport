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

package client

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
	// serverAddrSourceTimeout bounds a single lookup of the frps address.
	serverAddrSourceTimeout = 5 * time.Second
	// serverAddrSourceTTL is how long a resolved address is reused before it is
	// looked up again. Reconnects keep working from the cached value, so a
	// rotating address is picked up within this interval without hitting the
	// resolver on every dial.
	serverAddrSourceTTL = 30 * time.Second
)

// dialTarget is the host and port frpc dials for the frps control connection.
type dialTarget struct {
	host string
	port int
}

func (t dialTarget) address() string {
	return net.JoinHostPort(t.host, strconv.Itoa(t.port))
}

// serverAddrResolver resolves the frps address from serverAddrSource and keeps
// the last successful result as a fallback.
type serverAddrResolver struct {
	cfg     *v1.ClientCommonConfig
	source  *v1.ValueSource
	timeout time.Duration
	ttl     time.Duration
	// resolveValue fetches the raw value from the configured source. Tests
	// replace it to avoid network access.
	resolveValue func(context.Context) (string, error)

	mu       sync.Mutex
	cached   *dialTarget
	cachedAt time.Time
}

func newServerAddrResolver(cfg *v1.ClientCommonConfig) *serverAddrResolver {
	if cfg == nil || cfg.ServerAddrSource == nil {
		return nil
	}
	source := cfg.ServerAddrSource
	return &serverAddrResolver{
		cfg:          cfg,
		source:       source,
		timeout:      serverAddrSourceTimeout,
		ttl:          serverAddrSourceTTL,
		resolveValue: source.Resolve,
	}
}

// target returns the address to dial, resolving it again when the cached value
// has expired. Lookup failures never make the client unusable: the last known
// address, or the configured serverAddr/serverPort, is used instead.
func (r *serverAddrResolver) target(ctx context.Context) dialTarget {
	fallback := dialTarget{host: r.cfg.ServerAddr, port: r.cfg.ServerPort}
	xl := xlog.FromContextSafe(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Since(r.cachedAt) < r.ttl {
		return *r.cached
	}

	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := r.resolveValue(lookupCtx)
	if err != nil {
		if r.cached != nil {
			xl.Warnf("failed to refresh frps address from %s, reuse %s: %v",
				describeValueSource(r.source), r.cached.address(), err)
			return *r.cached
		}
		xl.Warnf("failed to resolve frps address from %s, fall back to %s: %v",
			describeValueSource(r.source), fallback.address(), err)
		return fallback
	}

	target, err := parseDialTarget(raw, fallback)
	if err != nil {
		if r.cached != nil {
			xl.Warnf("invalid frps address %q from %s, reuse %s: %v",
				raw, describeValueSource(r.source), r.cached.address(), err)
			return *r.cached
		}
		xl.Warnf("invalid frps address %q from %s, fall back to %s: %v",
			raw, describeValueSource(r.source), fallback.address(), err)
		return fallback
	}

	if r.cached == nil || *r.cached != target {
		xl.Infof("resolved frps address %s from %s", target.address(), describeValueSource(r.source))
	}
	r.cached = &target
	r.cachedAt = time.Now()
	return target
}

// parseDialTarget parses the value resolved from serverAddrSource. Supported
// forms are "host", "host:port", "[ipv6]" and "[ipv6]:port". A value without a
// port keeps the configured serverPort, a value without a host keeps the
// configured serverAddr.
func parseDialTarget(raw string, fallback dialTarget) (dialTarget, error) {
	value := firstAddressLine(raw)
	if value == "" {
		return dialTarget{}, errors.New("resolved value is empty")
	}

	host, portText := splitAddress(value)
	if host == "" {
		host = fallback.host
	}
	if host == "" {
		return dialTarget{}, fmt.Errorf("no host in %q", raw)
	}

	target := dialTarget{host: host, port: fallback.port}
	if portText == "" {
		return target, nil
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return dialTarget{}, fmt.Errorf("invalid port %q", portText)
	}
	target.port = port
	return target, nil
}

// firstAddressLine takes the first meaningful token of a resolved value, so
// multi-line values and trailing comments are tolerated.
func firstAddressLine(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		value := strings.Trim(fields[0], "\"'")
		if value != "" {
			return value
		}
	}
	return ""
}

// splitAddress splits "host[:port]" without requiring the host to be an IP, so
// domain names resolved from the source work as well.
func splitAddress(value string) (string, string) {
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
		// A bare IPv6 address; the port has to come from the configuration.
		return value, ""
	}
}

func describeValueSource(source *v1.ValueSource) string {
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
