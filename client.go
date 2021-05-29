/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package nri

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	types "github.com/ZentriaMC/nri/types/v1"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/oci"
	"github.com/pkg/errors"
)

const (
	// DefaultBinaryPath for nri plugins
	DefaultBinaryPath = "/opt/nri/bin"
	// DefaultConfPath for the global nri configuration
	DefaultConfPath = "/etc/nri/conf.json"
	// Version of NRI
	Version = "0.1"
)

// New nri client
func New() (*Client, error) {
	binPaths := append(filepath.SplitList(os.Getenv("PATH")), DefaultBinaryPath)
	return NewWithPaths(binPaths, DefaultConfPath)
}

// NewWithPaths creates a new NRI client with specified binary and
// configuration paths
func NewWithPaths(binPaths []string, confPath string) (*Client, error) {
	conf, err := loadConfig(confPath)
	if err != nil {
		return nil, err
	}

	return &Client{
		conf:     conf,
		binPaths: binPaths,
	}, nil
}

// Client for calling nri plugins
type Client struct {
	conf     *types.ConfigList
	binPaths []string
}

// Sandbox information
type Sandbox struct {
	// ID of the sandbox
	ID string
	// Labels of the sandbox
	Labels map[string]string
}

// InvokePre the ConfList of nri plugins before a runtime object has been created.
func (c *Client) InvokePre(ctx context.Context, container containerd.Container, sandbox *Sandbox) ([]*types.Result, error) {
	if len(c.conf.Plugins) == 0 {
		return nil, nil
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := createSpec(spec)
	if err != nil {
		return nil, err
	}
	r := &types.Request{
		Version: c.conf.Version,
		ID:      container.ID(),
		Pid:     -1,
		State:   types.PreCreate,
		Spec:    rs,
	}
	if sandbox != nil {
		r.SandboxID = sandbox.ID
		r.Labels = sandbox.Labels
	}
	for _, p := range c.conf.Plugins {
		r.Conf = p.Conf
		result, err := c.invokePlugin(ctx, p.Type, r)
		if err != nil {
			return nil, errors.Wrapf(err, "plugin: %s", p.Type)
		}
		r.Results = append(r.Results, result)
	}
	return r.Results, nil
}

// Invoke the ConfList of nri plugins
func (c *Client) Invoke(ctx context.Context, task containerd.Task, state types.State) ([]*types.Result, error) {
	return c.InvokeWithSandbox(ctx, task, state, nil)
}

// InvokeWithSandbox invokes the ConfList of nri plugins
func (c *Client) InvokeWithSandbox(ctx context.Context, task containerd.Task, state types.State, sandbox *Sandbox) ([]*types.Result, error) {
	if len(c.conf.Plugins) == 0 {
		return nil, nil
	}
	spec, err := task.Spec(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := createSpec(spec)
	if err != nil {
		return nil, err
	}
	r := &types.Request{
		Version: c.conf.Version,
		ID:      task.ID(),
		Pid:     int(task.Pid()),
		State:   state,
		Spec:    rs,
	}
	if sandbox != nil {
		r.SandboxID = sandbox.ID
		r.Labels = sandbox.Labels
	}
	for _, p := range c.conf.Plugins {
		r.Conf = p.Conf
		result, err := c.invokePlugin(ctx, p.Type, r)
		if err != nil {
			return nil, errors.Wrapf(err, "plugin: %s", p.Type)
		}
		r.Results = append(r.Results, result)
	}
	return r.Results, nil
}

func (c *Client) invokePlugin(ctx context.Context, name string, r *types.Request) (*types.Result, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}

	// Try to find the plugin path
	pluginPath, err := lookFromPaths(c.binPaths, name)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, pluginPath, "invoke")
	cmd.Stdin = bytes.NewBuffer(payload)
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	msg := "output:"
	if len(out) > 0 {
		msg = fmt.Sprintf("%s %q", msg, out)
	} else {
		msg = fmt.Sprintf("%s <empty>", msg)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// ExitError is returned when there is a non-zero exit status
			msg = fmt.Sprintf("%s exit code: %d", msg, exitErr.ExitCode())
		} else {
			// plugin did not get a chance to run, return exec err
			return nil, err
		}
	}
	var result types.Result
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, errors.Errorf("failed to unmarshal plugin output: %s: %s", err.Error(), msg)
	}
	if result.Err() != nil {
		return nil, result.Err()
	}
	return &result, nil
}

func loadConfig(path string) (*types.ConfigList, error) {
	f, err := os.Open(path)
	if err != nil {
		// if we don't have a config list on disk, create a new one for use
		if os.IsNotExist(err) {
			return &types.ConfigList{
				Version: Version,
			}, nil
		}
		return nil, err
	}
	var c types.ConfigList
	err = json.NewDecoder(f).Decode(&c)
	f.Close()
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func createSpec(spec *oci.Spec) (*types.Spec, error) {
	s := types.Spec{
		Namespaces:  make(map[string]string),
		Annotations: spec.Annotations,
	}
	switch {
	case spec.Linux != nil:
		s.CgroupsPath = spec.Linux.CgroupsPath
		data, err := json.Marshal(spec.Linux.Resources)
		if err != nil {
			return nil, err
		}
		s.Resources = json.RawMessage(data)
		for _, ns := range spec.Linux.Namespaces {
			s.Namespaces[string(ns.Type)] = ns.Path
		}
	case spec.Windows != nil:
		data, err := json.Marshal(spec.Windows.Resources)
		if err != nil {
			return nil, err
		}
		s.Resources = json.RawMessage(data)
	}
	return &s, nil
}

// findExecutable is copy from private os/exec.findExecutable
func findExecutable(file string) error {
	d, err := os.Stat(file)
	if err != nil {
		return err
	}
	if m := d.Mode(); !m.IsDir() && m&0111 != 0 {
		return nil
	}
	return fs.ErrPermission
}

// lookFromPaths is based on os/exec.LookPath, but allows specifying
// the paths where to look from, instead of relying on PATH environment
func lookFromPaths(paths []string, file string) (res string, err error) {
	if strings.Contains(file, "/") {
		err := findExecutable(file)
		if err == nil {
			return file, nil
		}
		return "", &exec.Error{Name: file, Err: err}
	}

	for _, dir := range paths {
		if dir == "" {
			// Unix shell semantics: path element "" means "."
			dir = "."
		}
		path := filepath.Join(dir, file)
		if err := findExecutable(path); err == nil {
			return path, nil
		}
	}
	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}
