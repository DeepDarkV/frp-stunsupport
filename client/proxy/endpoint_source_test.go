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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

func TestParseHostPort(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		fallbackHost string
		fallbackPort int
		want         hostPort
		wantErr      bool
	}{
		{
			name: "host and port",
			raw:  "203.0.113.10:18370",
			want: hostPort{host: "203.0.113.10", port: 18370},
		},
		{
			name:         "host only keeps the configured port",
			raw:          "backend.example.com",
			fallbackPort: 8080,
			want:         hostPort{host: "backend.example.com", port: 8080},
		},
		{
			name:         "port only keeps the configured host",
			raw:          ":9000",
			fallbackHost: "127.0.0.1",
			fallbackPort: 8080,
			want:         hostPort{host: "127.0.0.1", port: 9000},
		},
		{
			name: "ipv6 with port",
			raw:  "[2001:db8::1]:18370",
			want: hostPort{host: "2001:db8::1", port: 18370},
		},
		{
			name:         "bare ipv6",
			raw:          "2001:db8::1",
			fallbackPort: 8080,
			want:         hostPort{host: "2001:db8::1", port: 8080},
		},
		{
			name: "trailing comment",
			raw:  "203.0.113.10:18370 # mapped port",
			want: hostPort{host: "203.0.113.10", port: 18370},
		},
		{
			name: "quoted and multiline",
			raw:  "\n\"203.0.113.10:18370\"\n",
			want: hostPort{host: "203.0.113.10", port: 18370},
		},
		{name: "empty", raw: "  \n ", wantErr: true},
		{name: "port out of range", raw: "203.0.113.10:70000", wantErr: true},
		{name: "port not a number", raw: "203.0.113.10:http", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHostPort(tc.raw, tc.fallbackHost, tc.fallbackPort)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func newTestResolver(t *testing.T) *sourceResolver {
	t.Helper()
	r := newSourceResolver(&v1.ValueSource{Type: "txt", TXT: &v1.TXTSource{Record: "_backend.example.com"}})
	require.NotNil(t, r)
	return r
}

func TestSourceResolverCachesValue(t *testing.T) {
	r := newTestResolver(t)
	calls := 0
	r.lookup = func(context.Context) (string, error) {
		calls++
		return "203.0.113.10:18370", nil
	}

	got, err := r.resolve(context.Background(), "127.0.0.1", 8080)
	require.NoError(t, err)
	require.Equal(t, hostPort{host: "203.0.113.10", port: 18370}, got)

	// Second call inside the TTL must not hit the source again.
	got, err = r.resolve(context.Background(), "127.0.0.1", 8080)
	require.NoError(t, err)
	require.Equal(t, hostPort{host: "203.0.113.10", port: 18370}, got)
	require.Equal(t, 1, calls)
}

func TestSourceResolverFallsBackToLastValue(t *testing.T) {
	r := newTestResolver(t)
	calls := 0
	r.lookup = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "203.0.113.10:18370", nil
		}
		return "", errors.New("resolver down")
	}

	_, err := r.resolve(context.Background(), "127.0.0.1", 8080)
	require.NoError(t, err)

	r.ttl = -time.Second
	got, err := r.resolve(context.Background(), "127.0.0.1", 8080)
	require.NoError(t, err)
	require.Equal(t, hostPort{host: "203.0.113.10", port: 18370}, got)
	require.Equal(t, 2, calls)
}

func TestSourceResolverFallsBackToConfig(t *testing.T) {
	r := newTestResolver(t)
	r.lookup = func(context.Context) (string, error) {
		return "", errors.New("no such host")
	}
	got, err := r.resolve(context.Background(), "127.0.0.1", 8080)
	require.NoError(t, err)
	require.Equal(t, hostPort{host: "127.0.0.1", port: 8080}, got)
}

func TestSourceResolverPicksUpNewValue(t *testing.T) {
	r := newTestResolver(t)
	values := []string{"203.0.113.10:18370", "198.51.100.7:20777"}
	calls := 0
	r.lookup = func(context.Context) (string, error) {
		value := values[len(values)-1]
		if calls < len(values) {
			value = values[calls]
		}
		calls++
		return value, nil
	}

	got, _ := r.resolve(context.Background(), "", 0)
	require.Equal(t, 18370, got.port)

	r.ttl = -time.Second
	got, _ = r.resolve(context.Background(), "", 0)
	require.Equal(t, hostPort{host: "198.51.100.7", port: 20777}, got)
}

func TestSourceResolverResolvePort(t *testing.T) {
	r := newTestResolver(t)
	r.lookup = func(context.Context) (string, error) {
		return "203.0.113.10:18370", nil
	}
	port, err := r.resolvePort(context.Background(), 7000)
	require.NoError(t, err)
	require.Equal(t, 18370, port)

	// A value without a port keeps the configured one.
	r2 := newTestResolver(t)
	r2.lookup = func(context.Context) (string, error) { return "203.0.113.10", nil }
	port, err = r2.resolvePort(context.Background(), 7000)
	require.NoError(t, err)
	require.Equal(t, 7000, port)
}

func TestNewSourceResolverWithoutSource(t *testing.T) {
	require.Nil(t, newSourceResolver(nil))
}

func TestRemotePortSourceOf(t *testing.T) {
	source := &v1.ValueSource{Type: "txt", TXT: &v1.TXTSource{Record: "_open.example.com"}}

	tcpCfg := &v1.TCPProxyConfig{RemotePort: 7000, RemotePortSource: source}
	got, base := remotePortSourceOf(tcpCfg)
	require.Same(t, source, got)
	require.Equal(t, 7000, base)

	udpCfg := &v1.UDPProxyConfig{RemotePort: 7001, RemotePortSource: source}
	got, base = remotePortSourceOf(udpCfg)
	require.Same(t, source, got)
	require.Equal(t, 7001, base)

	// Other proxy types have no remote port to resolve.
	httpCfg := &v1.HTTPProxyConfig{}
	got, base = remotePortSourceOf(httpCfg)
	require.Nil(t, got)
	require.Equal(t, 0, base)
}
