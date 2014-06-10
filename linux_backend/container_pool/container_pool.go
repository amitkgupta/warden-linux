package container_pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/garden/warden"
	"github.com/cloudfoundry/gunk/command_runner"

	"github.com/cloudfoundry-incubator/warden-linux/linux_backend"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/bandwidth_manager"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/cgroups_manager"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/container_pool/rootfs_provider"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/network_pool"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/process_tracker"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/quota_manager"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/uid_pool"
)

var ErrUnknownRootFSProvider = errors.New("unknown rootfs provider")

type LinuxContainerPool struct {
	binPath   string
	depotPath string

	denyNetworks  []string
	allowNetworks []string

	rootfsProviders map[string]rootfs_provider.RootFSProvider

	uidPool     uid_pool.UIDPool
	networkPool network_pool.NetworkPool
	portPool    linux_backend.PortPool

	runner command_runner.CommandRunner

	quotaManager quota_manager.QuotaManager

	containerIDs chan string
}

func New(
	binPath, depotPath string,
	rootfsProviders map[string]rootfs_provider.RootFSProvider,
	uidPool uid_pool.UIDPool,
	networkPool network_pool.NetworkPool,
	portPool linux_backend.PortPool,
	denyNetworks, allowNetworks []string,
	runner command_runner.CommandRunner,
	quotaManager quota_manager.QuotaManager,
) *LinuxContainerPool {
	pool := &LinuxContainerPool{
		binPath:   binPath,
		depotPath: depotPath,

		rootfsProviders: rootfsProviders,

		allowNetworks: allowNetworks,
		denyNetworks:  denyNetworks,

		uidPool:     uidPool,
		networkPool: networkPool,
		portPool:    portPool,

		runner: runner,

		quotaManager: quotaManager,

		containerIDs: make(chan string),
	}

	go pool.generateContainerIDs()

	return pool
}

func (p *LinuxContainerPool) MaxContainers() int {
	maxNet := p.networkPool.InitialSize()
	maxUid := p.uidPool.InitialSize()
	if maxNet < maxUid {
		return maxNet
	}
	return maxUid
}

func (p *LinuxContainerPool) Setup() error {
	setup := &exec.Cmd{
		Path: path.Join(p.binPath, "setup.sh"),
		Env: []string{
			"POOL_NETWORK=" + p.networkPool.Network().String(),
			"DENY_NETWORKS=" + formatNetworks(p.denyNetworks),
			"ALLOW_NETWORKS=" + formatNetworks(p.allowNetworks),
			"CONTAINER_DEPOT_PATH=" + p.depotPath,
			"CONTAINER_DEPOT_MOUNT_POINT_PATH=" + p.quotaManager.MountPoint(),
			fmt.Sprintf("DISK_QUOTA_ENABLED=%v", p.quotaManager.IsEnabled()),
			"PATH=" + os.Getenv("PATH"),
		},
	}

	err := p.runner.Run(setup)
	if err != nil {
		return err
	}

	return nil
}

func formatNetworks(networks []string) string {
	return strings.Join(networks, " ")
}

func (p *LinuxContainerPool) Prune(keep map[string]bool) error {
	entries, err := ioutil.ReadDir(p.depotPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		id := entry.Name()
		if id == "tmp" {
			continue
		}

		_, found := keep[id]
		if found {
			continue
		}

		log.Println("pruning", id)

		err = p.destroy(id)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *LinuxContainerPool) Create(spec warden.ContainerSpec) (linux_backend.Container, error) {
	uid, err := p.uidPool.Acquire()
	if err != nil {
		return nil, err
	}

	network, err := p.networkPool.Acquire()
	if err != nil {
		p.uidPool.Release(uid)
		return nil, err
	}

	id := <-p.containerIDs

	containerPath := path.Join(p.depotPath, id)

	cgroupsManager := cgroups_manager.New(fmt.Sprintf("/tmp/warden%s/cgroup", os.Getenv("UNIQUENESS_TAG")), id)

	bandwidthManager := bandwidth_manager.New(containerPath, id, p.runner)

	handle := id
	if spec.Handle != "" {
		handle = spec.Handle
	}

	rootfsURL, err := url.Parse(spec.RootFSPath)
	if err != nil {
		return nil, err
	}

	provider, found := p.rootfsProviders[rootfsURL.Scheme]
	if !found {
		return nil, ErrUnknownRootFSProvider
	}

	rootfsPath, err := provider.ProvideRootFS(id, rootfsURL)
	if err != nil {
		return nil, err
	}

	container := linux_backend.NewLinuxContainer(
		id,
		handle,
		containerPath,
		spec.Properties,
		spec.GraceTime,
		linux_backend.NewResources(uid, network, []uint32{}),
		p.portPool,
		p.runner,
		cgroupsManager,
		p.quotaManager,
		bandwidthManager,
		process_tracker.New(containerPath, p.runner),
	)

	create := &exec.Cmd{
		Path: path.Join(p.binPath, "create.sh"),
		Args: []string{containerPath},
		Env: []string{
			"id=" + container.ID(),
			"rootfs_path=" + rootfsPath,
			fmt.Sprintf("user_uid=%d", uid),
			fmt.Sprintf("network_host_ip=%s", network.HostIP()),
			fmt.Sprintf("network_container_ip=%s", network.ContainerIP()),

			"PATH=" + os.Getenv("PATH"),
		},
	}

	err = p.runner.Run(create)
	if err != nil {
		p.uidPool.Release(uid)
		p.networkPool.Release(network)
		return nil, err
	}

	err = p.saveRootFSProvider(id, rootfsURL.Scheme)
	if err != nil {
		return nil, err
	}

	err = p.writeBindMounts(containerPath, spec.BindMounts)
	if err != nil {
		return nil, err
	}

	return container, nil
}

func (p *LinuxContainerPool) Restore(snapshot io.Reader) (linux_backend.Container, error) {
	var containerSnapshot linux_backend.ContainerSnapshot

	err := json.NewDecoder(snapshot).Decode(&containerSnapshot)
	if err != nil {
		return nil, err
	}

	id := containerSnapshot.ID

	log.Println("restoring", id)

	resources := containerSnapshot.Resources

	err = p.uidPool.Remove(resources.UID)
	if err != nil {
		return nil, err
	}

	err = p.networkPool.Remove(resources.Network)
	if err != nil {
		p.uidPool.Release(resources.UID)
		return nil, err
	}

	for _, port := range resources.Ports {
		err = p.portPool.Remove(port)
		if err != nil {
			p.uidPool.Release(resources.UID)
			p.networkPool.Release(resources.Network)

			for _, port := range resources.Ports {
				p.portPool.Release(port)
			}

			return nil, err
		}
	}

	containerPath := path.Join(p.depotPath, id)

	cgroupsManager := cgroups_manager.New(fmt.Sprintf("/tmp/warden%s/cgroup", os.Getenv("UNIQUENESS_TAG")), id)

	bandwidthManager := bandwidth_manager.New(containerPath, id, p.runner)

	container := linux_backend.NewLinuxContainer(
		id,
		containerSnapshot.Handle,
		containerPath,
		containerSnapshot.Properties,
		containerSnapshot.GraceTime,
		linux_backend.NewResources(
			resources.UID,
			resources.Network,
			resources.Ports,
		),
		p.portPool,
		p.runner,
		cgroupsManager,
		p.quotaManager,
		bandwidthManager,
		process_tracker.New(containerPath, p.runner),
	)

	err = container.Restore(containerSnapshot)
	if err != nil {
		return nil, err
	}

	return container, nil
}

func (p *LinuxContainerPool) Destroy(container linux_backend.Container) error {
	err := p.destroy(container.ID())
	if err != nil {
		return err
	}

	linuxContainer := container.(*linux_backend.LinuxContainer)

	resources := linuxContainer.Resources()

	for _, port := range resources.Ports {
		p.portPool.Release(port)
	}

	p.uidPool.Release(resources.UID)

	p.networkPool.Release(resources.Network)

	return nil
}

func (p *LinuxContainerPool) destroy(id string) error {
	rootfsProvider, err := ioutil.ReadFile(path.Join(p.depotPath, id, "rootfs-provider"))
	if err != nil {
		rootfsProvider = []byte("")
	}

	provider, found := p.rootfsProviders[string(rootfsProvider)]
	if !found {
		return ErrUnknownRootFSProvider
	}

	destroy := &exec.Cmd{
		Path: path.Join(p.binPath, "destroy.sh"),
		Args: []string{path.Join(p.depotPath, id)},
	}

	err = p.runner.Run(destroy)
	if err != nil {
		return err
	}

	return provider.CleanupRootFS(id)
}

func (p *LinuxContainerPool) generateContainerIDs() string {
	for containerNum := time.Now().UnixNano(); ; containerNum++ {
		containerID := []byte{}

		var i uint
		for i = 0; i < 11; i++ {
			containerID = strconv.AppendInt(
				containerID,
				(containerNum>>(55-(i+1)*5))&31,
				32,
			)
		}

		p.containerIDs <- string(containerID)
	}
}

func (p *LinuxContainerPool) writeBindMounts(
	containerPath string,
	bindMounts []warden.BindMount,
) error {
	hook := path.Join(containerPath, "lib", "hook-child-before-pivot.sh")

	for _, bm := range bindMounts {
		dstMount := path.Join(containerPath, "mnt", bm.DstPath)
		srcPath := bm.SrcPath

		if bm.Origin == warden.BindMountOriginContainer {
			srcPath = path.Join(containerPath, "tmp", "rootfs", srcPath)
		}

		mode := "ro"
		if bm.Mode == warden.BindMountModeRW {
			mode = "rw"
		}

		linebreak := &exec.Cmd{
			Path: "bash",
			Args: []string{
				"-c",
				"echo >> " + hook,
			},
		}

		err := p.runner.Run(linebreak)
		if err != nil {
			return err
		}

		mkdir := &exec.Cmd{
			Path: "bash",
			Args: []string{
				"-c",
				"echo mkdir -p " + dstMount + " >> " + hook,
			},
		}

		err = p.runner.Run(mkdir)
		if err != nil {
			return err
		}

		mount := &exec.Cmd{
			Path: "bash",
			Args: []string{
				"-c",
				"echo mount -n --bind " + srcPath + " " + dstMount +
					" >> " + hook,
			},
		}

		err = p.runner.Run(mount)
		if err != nil {
			return err
		}

		remount := &exec.Cmd{
			Path: "bash",
			Args: []string{
				"-c",
				"echo mount -n --bind -o remount," + mode + " " + srcPath + " " + dstMount +
					" >> " + hook,
			},
		}

		err = p.runner.Run(remount)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *LinuxContainerPool) saveRootFSProvider(id string, provider string) error {
	providerFile := path.Join(p.depotPath, id, "rootfs-provider")

	err := os.MkdirAll(path.Dir(providerFile), 0755)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(providerFile, []byte(provider), 0644)
}
