/*
Copyright 2025 The Kubernetes Authors All rights reserved.

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

package oci

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"k8s.io/klog/v2"
)

// SSHTunnelForContainer creates an SSH tunnel to access a container through a remote Docker host
type SSHTunnelForContainer struct {
	containerName string
	remoteHost    string
	remoteUser    string
	containerPort int
	localPort     int
	cmd           *exec.Cmd
	mu            sync.Mutex
}

var (
	// containerTunnels holds active SSH tunnels for containers
	containerTunnels = make(map[string]*SSHTunnelForContainer)
	tunnelsMux       sync.Mutex
)

// EstablishSSHTunnelForContainer creates an SSH tunnel to access container ports through remote Docker host
func EstablishSSHTunnelForContainer(containerName string, containerPort int) (int, error) {
	if !IsRemoteDockerContext() {
		return 0, fmt.Errorf("not using remote Docker context")
	}

	if !IsSSHDockerContext() {
		return 0, fmt.Errorf("remote Docker context is not SSH-based")
	}

	ctx, err := GetCurrentContext()
	if err != nil {
		return 0, errors.Wrap(err, "get current context")
	}

	// Extract SSH details from context host
	if !strings.HasPrefix(ctx.Host, "ssh://") {
		return 0, fmt.Errorf("Docker host is not SSH: %s", ctx.Host)
	}

	// Parse ssh://user@host format
	sshURL := strings.TrimPrefix(ctx.Host, "ssh://")
	parts := strings.Split(sshURL, "@")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid SSH endpoint format: %s", ctx.Host)
	}

	user := parts[0]
	host := parts[1]

	// Get the container's port on the remote host
	remotePort, err := ForwardedPort(Docker, containerName, containerPort)
	if err != nil {
		return 0, errors.Wrapf(err, "get forwarded port for container %s", containerName)
	}

	// Find available local port
	localPort, err := findAvailablePortForTunnel()
	if err != nil {
		return 0, errors.Wrap(err, "find available port")
	}

	// Create SSH tunnel
	tunnel := &SSHTunnelForContainer{
		containerName: containerName,
		remoteHost:    host,
		remoteUser:    user,
		containerPort: containerPort,
		localPort:     localPort,
	}

	if err := tunnel.Start(remotePort); err != nil {
		return 0, errors.Wrap(err, "start SSH tunnel")
	}

	// Store tunnel reference
	tunnelsMux.Lock()
	containerTunnels[containerName] = tunnel
	tunnelsMux.Unlock()

	klog.Infof("Established SSH tunnel for container %s: localhost:%d -> %s:127.0.0.1:%d", 
		containerName, localPort, host, remotePort)

	return localPort, nil
}

// findAvailablePortForTunnel finds an available local port to use for SSH tunneling
func findAvailablePortForTunnel() (int, error) {
	// Create a listener on port 0 to get a random available port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	
	// Get the port that was assigned
	addr := listener.Addr().(*net.TCPAddr)
	return addr.Port, nil
}

// Start begins the SSH tunnel
func (t *SSHTunnelForContainer) Start(remotePort int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// SSH command: ssh -N -L localPort:127.0.0.1:remotePort user@host
	args := []string{
		"-N", // Don't execute remote command
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=60",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("%d:127.0.0.1:%d", t.localPort, remotePort),
		fmt.Sprintf("%s@%s", t.remoteUser, t.remoteHost),
	}

	// Use SSH agent if available - don't specify key path

	t.cmd = exec.Command("ssh", args...)
	
	if err := t.cmd.Start(); err != nil {
		return errors.Wrap(err, "start SSH command")
	}

	// Wait for tunnel to be ready
	if err := t.waitForTunnel(); err != nil {
		t.Stop()
		return err
	}

	// Monitor tunnel health in background
	go t.monitor()

	return nil
}

// waitForTunnel waits for the SSH tunnel to be ready
func (t *SSHTunnelForContainer) waitForTunnel() error {
	for i := 0; i < 30; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", t.localPort), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for SSH tunnel to be ready")
}

// monitor checks tunnel health and restarts if needed
func (t *SSHTunnelForContainer) monitor() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		t.mu.Lock()
		if t.cmd == nil || t.cmd.Process == nil {
			t.mu.Unlock()
			return
		}

		// Check if process is still running
		if t.cmd.ProcessState != nil && t.cmd.ProcessState.Exited() {
			klog.Warningf("SSH tunnel for container %s died, attempting restart", t.containerName)
			t.mu.Unlock()
			
			// Try to restart
			remotePort, err := ForwardedPort(Docker, t.containerName, t.containerPort)
			if err != nil {
				klog.Errorf("Failed to get port for tunnel restart: %v", err)
				continue
			}
			
			if err := t.Start(remotePort); err != nil {
				klog.Errorf("Failed to restart SSH tunnel: %v", err)
			}
			continue
		}
		t.mu.Unlock()

		// Check tunnel connectivity
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", t.localPort), 5*time.Second)
		if err != nil {
			klog.Warningf("SSH tunnel for container %s not responding: %v", t.containerName, err)
		} else {
			conn.Close()
		}
	}
}

// Stop terminates the SSH tunnel
func (t *SSHTunnelForContainer) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cmd != nil && t.cmd.Process != nil {
		if err := t.cmd.Process.Kill(); err != nil {
			return errors.Wrap(err, "kill SSH process")
		}
		t.cmd.Wait()
		t.cmd = nil
	}
	return nil
}

// CleanupContainerTunnels stops all container SSH tunnels
func CleanupContainerTunnels() {
	tunnelsMux.Lock()
	defer tunnelsMux.Unlock()

	for name, tunnel := range containerTunnels {
		if err := tunnel.Stop(); err != nil {
			klog.Errorf("Failed to stop tunnel for container %s: %v", name, err)
		}
		delete(containerTunnels, name)
	}
}

// GetContainerSSHPort returns the local port for SSH access to a container
func GetContainerSSHPort(containerName string) (int, error) {
	if !IsRemoteDockerContext() {
		// For local Docker, use standard method
		return ForwardedPort(Docker, containerName, 22)
	}

	// Check if tunnel already exists
	tunnelsMux.Lock()
	tunnel, exists := containerTunnels[containerName]
	tunnelsMux.Unlock()

	if exists && tunnel != nil {
		return tunnel.localPort, nil
	}

	// Create new tunnel
	return EstablishSSHTunnelForContainer(containerName, 22)
}