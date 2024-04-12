// SPDX-License-Identifier: MPL-2.0
/*
 * Copyright (C) 2024 The Noisy Sockets Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package noisysockets_test

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/neilotoole/slogt"
	"github.com/noisysockets/noisysockets"
	"github.com/noisysockets/noisysockets/config"
	"github.com/noisysockets/noisysockets/config/v1alpha1"
	"github.com/noisysockets/noisysockets/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestNetwork(t *testing.T) {
	logger := slogt.New(t)

	serverPrivateKey, err := types.NewPrivateKey()
	require.NoError(t, err)

	clientPrivateKey, err := types.NewPrivateKey()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()

		// Wait for everything to close.
		time.Sleep(time.Second)
	})

	go func() {
		conf := v1alpha1.Config{
			Name:       "server",
			ListenPort: 12345,
			PrivateKey: serverPrivateKey.String(),
			IPs:        []string{"10.7.0.1"},
			Peers: []v1alpha1.PeerConfig{
				{
					PublicKey: clientPrivateKey.PublicKey().String(),
					IPs:       []string{"10.7.0.2"},
				},
			},
		}

		net, err := noisysockets.NewNetwork(logger, &conf)
		if err != nil {
			logger.Error("Failed to create server network", "error", err)
			return
		}
		defer net.Close()

		var mux http.ServeMux
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "Hello, world!")
		})

		srv := &http.Server{
			Handler: &mux,
		}
		defer srv.Close()

		// A little HTTP server.
		go func() {
			lis, err := net.Listen("tcp", ":80")
			if err != nil {
				logger.Error("Failed to listen", "error", err)
				return
			}
			defer lis.Close()

			if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("Failed to serve", "error", err)
				return
			}
		}()

		// A little UDP echo server.
		udpConn, err := net.ListenPacket("udp", "0.0.0.0:10000")
		if err != nil {
			logger.Error("Failed to listen", "error", err)
			return
		}
		defer udpConn.Close()

		go func() {
			buf := make([]byte, 1024)
			for {
				n, addr, err := udpConn.ReadFrom(buf)
				if err != nil {
					if errors.Is(err, io.EOF) {
						return
					}

					logger.Error("Failed to read", "error", err)
					return
				}

				if _, err := udpConn.WriteTo(buf[:n], addr); err != nil {
					logger.Error("Failed to write", "error", err)
					return
				}
			}
		}()

		<-ctx.Done()

		_ = srv.Close()
		_ = udpConn.Close()
	}()

	conf := v1alpha1.Config{
		Name:       "client",
		ListenPort: 12346,
		PrivateKey: clientPrivateKey.String(),
		IPs:        []string{"10.7.0.2"},
		Peers: []v1alpha1.PeerConfig{
			{
				Name:      "server",
				PublicKey: serverPrivateKey.PublicKey().String(),
				Endpoint:  "localhost:12345",
				IPs:       []string{"10.7.0.1"},
			},
		},
	}

	net, err := noisysockets.NewNetwork(logger, &conf)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, net.Close())
	})

	t.Run("TCP", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				Dial: net.Dial,
			},
		}

		// Wait for server to start.
		time.Sleep(time.Second)

		resp, err := client.Get("http://server")
		require.NoError(t, err)

		t.Cleanup(func() {
			require.NoError(t, resp.Body.Close())
		})

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.Equal(t, "Hello, world!", string(body))
	})

	t.Run("UDP", func(t *testing.T) {
		conn, err := net.Dial("udp", "server:10000")
		require.NoError(t, err)
		defer conn.Close()

		if _, err := conn.Write([]byte("Hello, world!")); err != nil {
			t.Fatal(err)
		}

		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		require.NoError(t, err)

		assert.Equal(t, "Hello, world!", string(buf[:n]))
	})
}

func TestWireGuardCompatibility(t *testing.T) {
	pwd, err := os.Getwd()
	require.NoError(t, err)

	ctx := context.Background()
	testNet, err := network.New(ctx, network.WithCheckDuplicate())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, testNet.Remove(ctx))
	})

	nginxReq := testcontainers.ContainerRequest{
		Image:        "nginx:latest",
		ExposedPorts: []string{"80/tcp"},
		Networks:     []string{testNet.Name},
		NetworkAliases: map[string][]string{
			testNet.Name: {"web"},
		},
		WaitingFor: wait.ForListeningPort("80/tcp"),
	}

	nginxC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: nginxReq,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, nginxC.Terminate(ctx))
	})

	// Spin up a WireGuard gateway.
	wgReq := testcontainers.ContainerRequest{
		Image:        "ghcr.io/noisysockets/gateway:v0.1.0",
		ExposedPorts: []string{"51820/udp"},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: filepath.Join(pwd, "testdata/wg0.conf"), ContainerFilePath: "/etc/wireguard/wg0.conf", FileMode: 0o400},
		},
		Networks: []string{testNet.Name},
		HostConfigModifier: func(hostConfig *container.HostConfig) {
			hostConfig.CapAdd = []string{"NET_ADMIN"}

			hostConfig.Sysctls = map[string]string{
				"net.ipv4.ip_forward":              "1",
				"net.ipv4.conf.all.src_valid_mark": "1",
			}

			hostConfig.Binds = append(hostConfig.Binds, "/dev/net/tun:/dev/net/tun")
		},
	}

	wgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: wgReq,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, wgC.Terminate(ctx))
	})

	outputDir := t.TempDir()
	configPath := filepath.Join(outputDir, "noisysockets.yaml")

	require.NoError(t, generateConfig(ctx, configPath, wgC))

	logger := slogt.New(t)

	configFile, err := os.Open(configPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, configFile.Close())
	})

	conf, err := config.FromYAML(configFile)
	require.NoError(t, err)

	net, err := noisysockets.NewNetwork(logger, conf)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, net.Close())
	})

	httpClient := &http.Client{
		Transport: &http.Transport{
			Dial: net.Dial,
		},
	}

	resp, err := httpClient.Get("http://web")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, resp.Body.Close())
	})

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func generateConfig(ctx context.Context, configPath string, wgC testcontainers.Container) error {
	wgHost, err := wgC.Host(ctx)
	if err != nil {
		return err
	}

	wgPort, err := wgC.MappedPort(ctx, "51820/udp")
	if err != nil {
		return err
	}

	var renderedConfig strings.Builder
	tmpl := template.Must(template.ParseFiles("testdata/noisysockets.yaml.tmpl"))
	if err := tmpl.Execute(&renderedConfig, struct {
		Endpoint string
	}{
		Endpoint: wgHost + ":" + wgPort.Port(),
	}); err != nil {
		return err
	}

	return os.WriteFile(configPath, []byte(renderedConfig.String()), 0o400)
}
