// Copyright 2025 The frp Authors
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

package v1

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// ValueSource provides a way to dynamically resolve configuration values
// from various sources like files, environment variables, or external services.
type ValueSource struct {
	Type string      `json:"type"`
	File *FileSource `json:"file,omitempty"`
	Exec *ExecSource `json:"exec,omitempty"`
	TXT  *TXTSource  `json:"txt,omitempty"`
}

// TXTSource specifies how to read a value from a DNS TXT record. The lookup uses
// the system resolver, or the DNS server configured with dnsServer.
// The multiple character-strings of one TXT record are concatenated by the
// resolver; if the name carries several TXT records, the first non-empty one is
// used.
type TXTSource struct {
	Record string `json:"record"`
}

// FileSource specifies how to load a value from a file.
type FileSource struct {
	Path string `json:"path"`
}

// ExecSource specifies how to get a value from another program launched as subprocess.
type ExecSource struct {
	Command string       `json:"command"`
	Args    []string     `json:"args,omitempty"`
	Env     []ExecEnvVar `json:"env,omitempty"`
}

type ExecEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Validate validates the ValueSource configuration.
func (v *ValueSource) Validate() error {
	if v == nil {
		return errors.New("valueSource cannot be nil")
	}

	switch v.Type {
	case "file":
		if v.File == nil {
			return errors.New("file configuration is required when type is 'file'")
		}
		return v.File.Validate()
	case "exec":
		if v.Exec == nil {
			return errors.New("exec configuration is required when type is 'exec'")
		}
		return v.Exec.Validate()
	case "txt":
		if v.TXT == nil {
			return errors.New("txt configuration is required when type is 'txt'")
		}
		return v.TXT.Validate()
	default:
		return fmt.Errorf("unsupported value source type: %s (only 'file', 'exec' and 'txt' are supported)", v.Type)
	}
}

// Resolve resolves the value from the configured source.
func (v *ValueSource) Resolve(ctx context.Context) (string, error) {
	if err := v.Validate(); err != nil {
		return "", err
	}

	switch v.Type {
	case "file":
		return v.File.Resolve(ctx)
	case "exec":
		return v.Exec.Resolve(ctx)
	case "txt":
		return v.TXT.Resolve(ctx)
	default:
		return "", fmt.Errorf("unsupported value source type: %s", v.Type)
	}
}

// Validate validates the FileSource configuration.
func (f *FileSource) Validate() error {
	if f == nil {
		return errors.New("fileSource cannot be nil")
	}

	if f.Path == "" {
		return errors.New("file path cannot be empty")
	}
	return nil
}

// Resolve reads and returns the content from the specified file.
func (f *FileSource) Resolve(_ context.Context) (string, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}

	content, err := os.ReadFile(f.Path)
	if err != nil {
		return "", fmt.Errorf("failed to read file %s: %v", f.Path, err)
	}

	// Trim whitespace, which is important for file-based tokens
	return strings.TrimSpace(string(content)), nil
}

// Validate validates the ExecSource configuration.
func (e *ExecSource) Validate() error {
	if e == nil {
		return errors.New("execSource cannot be nil")
	}

	if e.Command == "" {
		return errors.New("exec command cannot be empty")
	}

	for _, env := range e.Env {
		if env.Name == "" {
			return errors.New("exec env name cannot be empty")
		}
		if strings.Contains(env.Name, "=") {
			return errors.New("exec env name cannot contain '='")
		}
	}
	return nil
}

// Resolve reads and returns the content captured from stdout of launched subprocess.
func (e *ExecSource) Resolve(ctx context.Context) (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, e.Command, e.Args...)
	if len(e.Env) != 0 {
		cmd.Env = os.Environ()
		for _, env := range e.Env {
			cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
		}
	}

	content, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to execute command %v: %v", e.Command, err)
	}

	// Trim whitespace, which is important for exec-based tokens
	return strings.TrimSpace(string(content)), nil
}

// Validate validates the TXTSource configuration.
func (t *TXTSource) Validate() error {
	if t == nil {
		return errors.New("txtSource cannot be nil")
	}

	record := NormalizeTXTRecord(t.Record)
	if record == "" {
		return errors.New("txt record cannot be empty")
	}
	if strings.ContainsAny(record, " \t\r\n") {
		return fmt.Errorf("invalid txt record: %q", t.Record)
	}
	return nil
}

// Resolve looks up the configured TXT record and returns its first non-empty
// value.
func (t *TXTSource) Resolve(ctx context.Context) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}

	record := NormalizeTXTRecord(t.Record)
	values, err := net.DefaultResolver.LookupTXT(ctx, record)
	if err != nil {
		return "", fmt.Errorf("failed to look up TXT record %s: %v", record, err)
	}

	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed, nil
		}
	}
	return "", fmt.Errorf("TXT record %s is empty", record)
}

// NormalizeTXTRecord trims spaces and the trailing root dot of a TXT record
// name, so that both "_frp.example.com" and "_frp.example.com." work.
func NormalizeTXTRecord(record string) string {
	return strings.TrimSuffix(strings.TrimSpace(record), ".")
}
