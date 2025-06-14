/*
Copyright 2024 The Kubernetes Authors All rights reserved.

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
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"k8s.io/klog/v2"
)

// TunnelManager manages SSH tunnels for remote Docker contexts
type TunnelManager struct {
	tunnels map[string]*SSHTunnel
	mutex   sync.RWMutex
}

// SSHTunnel represents an active SSH tunnel
type SSHTunnel struct {
	LocalPort  int
	RemoteHost string
	RemotePort int
	SSHHost    string
	SSHUser    string
	SSHPort    int
	Process    *exec.Cmd
	Cancel     context.CancelFunc
	Status     string
}

var (
	globalTunnelManager *TunnelManager
	tunnelManagerOnce   sync.Once
)

// GetTunnelManager returns the global tunnel manager instance
func GetTunnelManager() *TunnelManager {
	tunnelManagerOnce.Do(func() {
		globalTunnelManager = &TunnelManager{
			tunnels: make(map[string]*SSHTunnel),
		}
	})
	return globalTunnelManager
}

// CreateAPIServerTunnel creates an SSH tunnel for API server access
func (tm *TunnelManager) CreateAPIServerTunnel(ctx *ContextInfo, remotePort int) (*SSHTunnel, error) {
	if !ctx.IsSSH {
		return nil, errors.New("context is not SSH-based")
	}

	u, err := url.Parse(ctx.Host)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing SSH host %q", ctx.Host)
	}

	sshUser := u.User.Username()
	sshHost := u.Hostname()
	sshPort := 22
	if u.Port() != "" {
		sshPort, _ = strconv.Atoi(u.Port())
	}

	// Find available local port
	localPort, err := findAvailablePort()
	if err != nil {
		return nil, errors.Wrap(err, "finding available local port")
	}

	tunnelKey := fmt.Sprintf("%s:%d->%s:%d", sshHost, remotePort, "localhost", localPort)

	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	// Check if tunnel already exists
	if existing, exists := tm.tunnels[tunnelKey]; exists {
		if existing.Status == "active" {
			klog.Infof("SSH tunnel already active: %s", tunnelKey)
			return existing, nil
		}
		// Clean up stale tunnel
		tm.cleanupTunnel(existing)
		delete(tm.tunnels, tunnelKey)
	}

	tunnel := &SSHTunnel{
		LocalPort:  localPort,
		RemoteHost: "localhost", // API server runs on localhost inside the remote container
		RemotePort: remotePort,
		SSHHost:    sshHost,
		SSHUser:    sshUser,
		SSHPort:    sshPort,
		Status:     "starting",
	}

	if err := tm.startTunnel(tunnel); err != nil {
		return nil, errors.Wrapf(err, "starting SSH tunnel %s", tunnelKey)
	}

	tm.tunnels[tunnelKey] = tunnel
	klog.Infof("SSH tunnel created: %s", tunnelKey)

	return tunnel, nil
}

// startTunnel starts the SSH tunnel process
func (tm *TunnelManager) startTunnel(tunnel *SSHTunnel) error {
	ctx, cancel := context.WithCancel(context.Background())
	tunnel.Cancel = cancel

	// Build SSH command
	sshArgs := []string{
		"-L", fmt.Sprintf("%d:%s:%d", tunnel.LocalPort, tunnel.RemoteHost, tunnel.RemotePort),
		"-N", // Don't execute remote command
		"-f", // Run in background
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=yes",
		fmt.Sprintf("%s@%s", tunnel.SSHUser, tunnel.SSHHost),
	}

	if tunnel.SSHPort != 22 {
		sshArgs = append([]string{"-p", strconv.Itoa(tunnel.SSHPort)}, sshArgs...)
	}

	klog.V(3).Infof("Starting SSH tunnel: ssh %s", strings.Join(sshArgs, " "))

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stderr = os.Stderr

	tunnel.Process = cmd

	if err := cmd.Start(); err != nil {
		cancel()
		return errors.Wrap(err, "starting SSH process")
	}

	// Wait for tunnel to be ready
	if err := tm.waitForTunnel(tunnel, 10*time.Second); err != nil {
		cancel()
		cmd.Process.Kill()
		return errors.Wrap(err, "waiting for tunnel to be ready")
	}

	tunnel.Status = "active"

	// Monitor tunnel in background
	go tm.monitorTunnel(tunnel)

	return nil
}

// waitForTunnel waits for the SSH tunnel to be ready
func (tm *TunnelManager) waitForTunnel(tunnel *SSHTunnel, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tunnel.LocalPort), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return errors.New("tunnel did not become ready within timeout")
}

// monitorTunnel monitors the tunnel process and restarts if needed
func (tm *TunnelManager) monitorTunnel(tunnel *SSHTunnel) {
	defer func() {
		tm.mutex.Lock()
		tunnel.Status = "stopped"
		tm.mutex.Unlock()
	}()

	// Start health checking in a separate goroutine
	go tm.healthCheckTunnel(tunnel)

	if err := tunnel.Process.Wait(); err != nil {
		klog.Warningf("SSH tunnel process exited with error: %v", err)
		
		// Attempt auto-recovery if the tunnel was active
		if tunnel.Status == "active" {
			klog.Infof("Attempting to restart SSH tunnel...")
			if err := tm.restartTunnel(tunnel); err != nil {
				klog.Errorf("Failed to restart SSH tunnel: %v", err)
			}
		}
	} else {
		klog.Infof("SSH tunnel process exited cleanly")
	}
}

// healthCheckTunnel periodically checks tunnel health
func (tm *TunnelManager) healthCheckTunnel(tunnel *SSHTunnel) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if tunnel.Status != "active" {
				return // Tunnel is no longer active
			}

			// Perform comprehensive health check
			if err := tm.performHealthCheck(tunnel); err != nil {
				klog.Warningf("SSH tunnel health check failed: %v", err)
				tunnel.Status = "unhealthy"
			} else {
				if tunnel.Status == "unhealthy" {
					klog.Infof("SSH tunnel health restored")
				}
				tunnel.Status = "active"
			}

		case <-time.After(5 * time.Minute):
			// Stop health checking after 5 minutes
			return
		}
	}
}

// restartTunnel attempts to restart a failed tunnel
func (tm *TunnelManager) restartTunnel(tunnel *SSHTunnel) error {
	klog.Infof("Restarting SSH tunnel to %s:%d", tunnel.SSHHost, tunnel.RemotePort)
	
	// Clean up the old process
	tm.cleanupTunnel(tunnel)
	
	// Wait a moment before restarting
	time.Sleep(2 * time.Second)
	
	// Start the tunnel again
	return tm.startTunnel(tunnel)
}

// cleanupTunnel cleans up a tunnel
func (tm *TunnelManager) cleanupTunnel(tunnel *SSHTunnel) {
	if tunnel.Cancel != nil {
		tunnel.Cancel()
	}
	if tunnel.Process != nil && tunnel.Process.Process != nil {
		tunnel.Process.Process.Kill()
	}
}

// StopTunnel stops a specific tunnel
func (tm *TunnelManager) StopTunnel(tunnelKey string) error {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	tunnel, exists := tm.tunnels[tunnelKey]
	if !exists {
		return errors.Errorf("tunnel %s not found", tunnelKey)
	}

	tm.cleanupTunnel(tunnel)
	delete(tm.tunnels, tunnelKey)

	klog.Infof("SSH tunnel stopped: %s", tunnelKey)
	return nil
}

// StopAllTunnels stops all active tunnels
func (tm *TunnelManager) StopAllTunnels() {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	for key, tunnel := range tm.tunnels {
		tm.cleanupTunnel(tunnel)
		delete(tm.tunnels, key)
	}

	klog.Infof("All SSH tunnels stopped")
}

// GetActiveTunnels returns information about active tunnels
func (tm *TunnelManager) GetActiveTunnels() map[string]*SSHTunnel {
	tm.mutex.RLock()
	defer tm.mutex.RUnlock()

	result := make(map[string]*SSHTunnel)
	for key, tunnel := range tm.tunnels {
		if tunnel.Status == "active" {
			result[key] = tunnel
		}
	}

	return result
}

// findAvailablePort finds an available local port
func findAvailablePort() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	return addr.Port, nil
}

// GetAPIServerTunnelEndpoint returns the local endpoint for API server access
func GetAPIServerTunnelEndpoint(ctx *ContextInfo, apiServerPort int) (string, error) {
	if !ctx.IsRemote {
		return "", nil // No tunnel needed for local contexts
	}

	if !ctx.IsSSH {
		return "", errors.New("automatic tunneling only supported for SSH contexts")
	}

	tm := GetTunnelManager()
	tunnel, err := tm.CreateAPIServerTunnel(ctx, apiServerPort)
	if err != nil {
		return "", errors.Wrap(err, "creating API server tunnel")
	}

	return fmt.Sprintf("https://localhost:%d", tunnel.LocalPort), nil
}

// performHealthCheck performs comprehensive health check for SSH tunnels
func (tm *TunnelManager) performHealthCheck(tunnel *SSHTunnel) error {
	// 1. Check if local tunnel port is responsive
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tunnel.LocalPort), 2*time.Second)
	if err != nil {
		return errors.Wrap(err, "local tunnel port not responsive")
	}
	conn.Close()

	// 2. Verify SSH connectivity to remote host
	if err := tm.checkSSHConnectivity(tunnel); err != nil {
		return errors.Wrap(err, "SSH connectivity check failed")
	}

	// 3. Verify remote service is accessible through tunnel
	if err := tm.checkRemoteServiceConnectivity(tunnel); err != nil {
		klog.V(3).Infof("Remote service connectivity check failed (may be normal): %v", err)
		// Don't fail health check for remote service issues as the tunnel itself may be fine
	}

	return nil
}

// checkSSHConnectivity verifies SSH connection to remote host
func (tm *TunnelManager) checkSSHConnectivity(tunnel *SSHTunnel) error {
	// Use a quick SSH command to verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sshArgs := []string{
		"-o", "ConnectTimeout=3",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes", // Don't prompt for passwords
		fmt.Sprintf("%s@%s", tunnel.SSHUser, tunnel.SSHHost),
		"echo", "ssh-health-check",
	}

	if tunnel.SSHPort != 22 {
		sshArgs = append([]string{"-p", strconv.Itoa(tunnel.SSHPort)}, sshArgs...)
	}

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return errors.Wrapf(err, "SSH command failed: %s", string(output))
	}

	// Verify we got expected response
	if !strings.Contains(string(output), "ssh-health-check") {
		return errors.Errorf("unexpected SSH response: %s", string(output))
	}

	return nil
}

// checkRemoteServiceConnectivity verifies the remote service is accessible
func (tm *TunnelManager) checkRemoteServiceConnectivity(tunnel *SSHTunnel) error {
	// Try to connect to the remote service through the tunnel
	// This is a best-effort check since the service might not be ready
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Use curl through SSH to check if the remote service responds
	sshArgs := []string{
		"-o", "ConnectTimeout=2",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes",
		fmt.Sprintf("%s@%s", tunnel.SSHUser, tunnel.SSHHost),
		"curl", "-k", "--connect-timeout", "2", "--max-time", "3",
		"--silent", "--output", "/dev/null", "--write-out", "%{http_code}",
		fmt.Sprintf("https://%s:%d/", tunnel.RemoteHost, tunnel.RemotePort),
	}

	if tunnel.SSHPort != 22 {
		sshArgs = append([]string{"-p", strconv.Itoa(tunnel.SSHPort)}, sshArgs...)
	}

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return errors.Wrapf(err, "remote service check failed: %s", string(output))
	}

	// Any HTTP response code indicates the service is reachable
	// Even 404 or 401 means the service is responding
	responseCode := strings.TrimSpace(string(output))
	if responseCode == "" || responseCode == "000" {
		return errors.New("remote service not responding")
	}

	klog.V(3).Infof("Remote service health check: HTTP %s", responseCode)
	return nil
}