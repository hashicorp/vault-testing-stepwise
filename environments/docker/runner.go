// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/moby/go-archive"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Runner manages the lifecycle of the Docker container
type Runner struct {
	dockerAPI       *client.Client
	ContainerConfig *container.Config
	ContainerName   string
	NetName         string
	IP              string
	CopyFromTo      map[string]string
}

// Start is responsible for executing the Vault container. It consists of
// pulling the specified Vault image, creating the container, and copies the
// plugin binary into the container file system before starting the container
// itself.
func (d *Runner) Start(ctx context.Context) (*container.InspectResponse, error) {
	hostConfig := &container.HostConfig{
		PublishAllPorts: true,
		AutoRemove:      true,
	}

	networkingConfig := &network.NetworkingConfig{}
	switch d.NetName {
	case "":
	case "host":
		hostConfig.NetworkMode = "host"
	default:
		es := &network.EndpointSettings{
			Aliases: []string{d.ContainerName},
		}
		if len(d.IP) != 0 {
			addr, err := netip.ParseAddr(d.IP)
			if err != nil {
				return nil, fmt.Errorf("invalid IP address %q: %v", d.IP, err)
			}
			es.IPAMConfig = &network.EndpointIPAMConfig{
				IPv4Address: addr,
			}
		}
		networkingConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			d.NetName: es,
		}
	}

	// Best-effort pull. If the image already exists locally this is a no-op.
	// Pull errors are intentionally ignored so that a locally cached image is
	// used when the registry is unreachable or the image is not found there.
	if pullResp, err := d.dockerAPI.ImagePull(ctx, d.ContainerConfig.Image, client.ImagePullOptions{}); err == nil {
		_ = pullResp.Wait(ctx)
	}

	cfg := *d.ContainerConfig
	hostConfig.CapAdd = []string{"IPC_LOCK"}
	cfg.Hostname = d.ContainerName
	newContainer, err := d.dockerAPI.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             d.ContainerName,
		Config:           &cfg,
		HostConfig:       hostConfig,
		NetworkingConfig: networkingConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("container create failed: %v", err)
	}

	// copies the plugin binary into the Docker file system. This copy is only
	// allowed before the container is started
	for from, to := range d.CopyFromTo {
		srcInfo, err := archive.CopyInfoSourcePath(from, false)
		if err != nil {
			return nil, fmt.Errorf("error copying from source %q: %v", from, err)
		}

		srcArchive, err := archive.TarResource(srcInfo)
		if err != nil {
			return nil, fmt.Errorf("error creating tar from source %q: %v", from, err)
		}
		defer srcArchive.Close()

		dstInfo := archive.CopyInfo{Path: to}

		dstDir, content, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, dstInfo)
		if err != nil {
			return nil, fmt.Errorf("error preparing copy from %q -> %q: %v", from, to, err)
		}
		defer content.Close()
		_, err = d.dockerAPI.CopyToContainer(ctx, newContainer.ID, client.CopyToContainerOptions{
			DestinationPath: dstDir,
			Content:         content,
		})
		if err != nil {
			return nil, fmt.Errorf("error copying from %q -> %q: %v", from, to, err)
		}
	}

	_, err = d.dockerAPI.ContainerStart(ctx, newContainer.ID, client.ContainerStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("container start failed: %v", err)
	}

	inspectResult, err := d.dockerAPI.ContainerInspect(ctx, newContainer.ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return &inspectResult.Container, nil
}
