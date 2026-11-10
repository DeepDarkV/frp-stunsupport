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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

func TestParseDialTarget(t *testing.T) {
	fallback := dialTarget{host: "frp.example.com", port: 7000}

	tests := []struct {
		name    string
		raw     string
		want    dialTarget
		wantErr bool
	}{
		{
			name: "host and port",
			raw:  "203.0.113.10:17902",
			want: dialTarget{host: "203.0.113.10", port: 17902},
		},
		{
			name: "host only keeps the configured port",
			raw:  "203.0.113.10",
			want: dialTarget{host: "203.0.113.10", port: 7000},
		},
		{
			name: "domain with port",
			raw:  "frps.example.com:7443",
			want: dialTarget{host: "frps.example.com", port: 7443},
		},
		{
			name: "ipv6 with port",
			raw:  "[2001:db8::1]:17902",
			want: dialTarget{host: "2001:db8::1", port: 17902},
		},
		{
			name: "bare ipv6 without port",
			raw:  "2001:db8::1",
			want: dialTarget{host: "2001:db8::1", port: 7000},
		},
		{
			name: "trailing comment",
			raw:  "203.0.113.10:17902 # stun mapping",
			want: dialTarget{host: "203.0.113.10", port: 17902},
		},
		{
			name: "surrounding whitespace and newlines",
			raw:  "\n  203.0.113.10:17902  \n",
			want: dialTarget{host: "203.0.113.10", port: 17902},
		},
		{
			name: "quoted value",
			raw:  "\"203.0.113.10:17902\"",
			want: dialTarget{host: "203.0.113.10", port: 17902},
		},
		{
			name:    "empty value",
			raw:     "   \n ",
			wantErr: true,
		},
		{
			name:    "port out of range",
			raw:     "203.0.113.10:99999",
			wantErr: true,
		},
		{
			name:    "port is not a number",
			raw:     "203.0.113.10:abc",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDialTarget(tc.raw, fallback)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseDialTargetWithoutHost(t *testing.T) {
	// A value that carries only a port is not useful on its own.
	_, err := parseDialTarget(":7000", dialTarget{host: "", port: 7000})
	require.Error(t, err)
}

func newTestResolver(t *testing.T, cfg *v1.ClientCommonConfig) *serverAddrResolver {
	t.Helper()
	r := newServerAddrResolver(cfg)
	require.NotNil(t, r)
	return r
}

func TestServerAddrResolverCachesResolvedValue(t *testing.T) {
	cfg := &v1.ClientCommonConfig{ServerAddr: "frp.example.com", ServerPort: 7000}
	cfg.ServerAddrSource = &v1.ValueSource{Type: "txt", TXT: &v1.TXTSource{Record: "_frp.example.com"}}
	r := newTestResolver(t, cfg)

	calls := 0
	r.resolveValue = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "203.0.113.10:17902", nil
		}
		return "", errors.New("resolver unavailable")
	}

	require.Equal(t, dialTarget{host: "203.0.113.10", port: 17902}, r.target(context.Background()))
	// The value is reused inside the TTL, so a reconnect does not hit DNS again.
	require.Equal(t, dialTarget{host: "203.0.113.10", port: 17902}, r.target(context.Background()))
	require.Equal(t, 1, calls)
}

func TestServerAddrResolverKeepsLastKnownTargetOnFailure(t *testing.T) {
	cfg := &v1.ClientCommonConfig{ServerAddr: "frp.example.com", ServerPort: 7000}
	cfg.ServerAddrSource = &v1.ValueSource{Type: "txt", TXT: &v1.TXTSource{Record: "_frp.example.com"}}
	r := newTestResolver(t, cfg)

	calls := 0
	r.resolveValue = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "203.0.113.10:17902", nil
		}
		return "", errors.New("resolver unavailable")
	}
	require.Equal(t, dialTarget{host: "203.0.113.10", port: 17902}, r.target(context.Background()))

	r.ttl = -time.Second
	require.Equal(t, dialTarget{host: "203.0.113.10", port: 17902}, r.target(context.Background()))
	require.Equal(t, 2, calls)
}

func TestServerAddrResolverFallsBackToConfig(t *testing.T) {
	cfg := &v1.ClientCommonConfig{ServerAddr: "frp.example.com", ServerPort: 7000}
	cfg.ServerAddrSource = &v1.ValueSource{Type: "txt", TXT: &v1.TXTSource{Record: "_frp.example.com"}}
	r := newTestResolver(t, cfg)

	r.resolveValue = func(context.Context) (string, error) {
		return "", errors.New("no such host")
	}
	require.Equal(t, dialTarget{host: "frp.example.com", port: 7000}, r.target(context.Background()))

	// An unusable value is treated like a failed lookup.
	r.resolveValue = func(context.Context) (string, error) {
		return "203.0.113.10:99999", nil
	}
	require.Equal(t, dialTarget{host: "frp.example.com", port: 7000}, r.target(context.Background()))
}

func TestServerAddrResolverRefreshesExpiredValue(t *testing.T) {
	cfg := &v1.ClientCommonConfig{ServerAddr: "frp.example.com", ServerPort: 7000}
	cfg.ServerAddrSource = &v1.ValueSource{Type: "txt", TXT: &v1.TXTSource{Record: "_frp.example.com"}}
	r := newTestResolver(t, cfg)

	values := []string{"203.0.113.10:17902", "198.51.100.7:20777"}
	calls := 0
	r.resolveValue = func(context.Context) (string, error) {
		value := values[len(values)-1]
		if calls < len(values) {
			value = values[calls]
		}
		calls++
		return value, nil
	}

	require.Equal(t, dialTarget{host: "203.0.113.10", port: 17902}, r.target(context.Background()))
	// Simulate the gateway rotating its published port.
	r.ttl = -time.Second
	require.Equal(t, dialTarget{host: "198.51.100.7", port: 20777}, r.target(context.Background()))
}

func TestNewServerAddrResolverWithoutSource(t *testing.T) {
	require.Nil(t, newServerAddrResolver(nil))
	require.Nil(t, newServerAddrResolver(&v1.ClientCommonConfig{}))
}

func TestConnectorDialTargetWithoutSource(t *testing.T) {
	cfg := &v1.ClientCommonConfig{ServerAddr: "127.0.0.1", ServerPort: 7000}
	c, ok := NewConnector(context.Background(), cfg).(*defaultConnectorImpl)
	require.True(t, ok)
	require.Nil(t, c.addr)
	require.Equal(t, dialTarget{host: "127.0.0.1", port: 7000}, c.dialTarget(context.Background()))
}

func TestConnectorServerNamePrefersConfiguredAddr(t *testing.T) {
	cfg := &v1.ClientCommonConfig{ServerAddr: "frp.example.com", ServerPort: 7000}
	cfg.ServerAddrSource = &v1.ValueSource{Type: "txt", TXT: &v1.TXTSource{Record: "_frp.example.com"}}
	c, ok := NewConnector(context.Background(), cfg).(*defaultConnectorImpl)
	require.True(t, ok)
	require.Equal(t, "frp.example.com", c.serverName(dialTarget{host: "203.0.113.10", port: 17902}))

	// Without a configured serverAddr the resolved host is used as the SNI name.
	cfg.ServerAddr = ""
	require.Equal(t, "203.0.113.10", c.serverName(dialTarget{host: "203.0.113.10", port: 17902}))
}
