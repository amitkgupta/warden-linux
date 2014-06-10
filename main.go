package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/dotcloud/docker/daemon/graphdriver"
	_ "github.com/dotcloud/docker/daemon/graphdriver/aufs"
	_ "github.com/dotcloud/docker/daemon/graphdriver/vfs"
	"github.com/dotcloud/docker/graph"
	"github.com/dotcloud/docker/registry"

	"github.com/cloudfoundry-incubator/garden/server"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/container_pool"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/container_pool/repository_fetcher"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/container_pool/rootfs_provider"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/network_pool"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/port_pool"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/quota_manager"
	"github.com/cloudfoundry-incubator/warden-linux/linux_backend/uid_pool"
	"github.com/cloudfoundry-incubator/warden-linux/system_info"
	"github.com/cloudfoundry/gunk/command_runner"
	"github.com/cloudfoundry/gunk/command_runner/linux_command_runner"
)

type potato struct {
	tag string
	command_runner.CommandRunner
}

func (p *potato) Run(cmd *exec.Cmd) error {
	if len(cmd.Env) == 0 {
		cmd.Env = append(os.Environ(), "UNIQUENESS_TAG="+p.tag)
	} else {
		cmd.Env = append(cmd.Env, "UNIQUENESS_TAG="+p.tag)
	}

	return p.CommandRunner.Run(cmd)
}

func (p *potato) Start(cmd *exec.Cmd) error {
	if len(cmd.Env) == 0 {
		cmd.Env = append(os.Environ(), "UNIQUENESS_TAG="+p.tag)
	} else {
		cmd.Env = append(cmd.Env, "UNIQUENESS_TAG="+p.tag)
	}

	return p.CommandRunner.Start(cmd)
}

func (p *potato) Background(cmd *exec.Cmd) error {
	if len(cmd.Env) == 0 {
		cmd.Env = append(os.Environ(), "UNIQUENESS_TAG="+p.tag)
	} else {
		cmd.Env = append(cmd.Env, "UNIQUENESS_TAG="+p.tag)
	}

	return p.CommandRunner.Background(cmd)
}

var listenNetwork = flag.String(
	"listenNetwork",
	"unix",
	"how to listen on the address (unix, tcp, etc.)",
)

var listenAddr = flag.String(
	"listenAddr",
	"/tmp/warden.sock",
	"address to listen on",
)

var snapshotsPath = flag.String(
	"snapshots",
	"",
	"directory in which to store container state to persist through restarts",
)

var binPath = flag.String(
	"bin",
	"",
	"directory containing backend-specific scripts (i.e. ./create.sh)",
)

var depotPath = flag.String(
	"depot",
	"",
	"directory in which to store containers",
)

var overlaysPath = flag.String(
	"overlays",
	"",
	"directory in which to store containers mount points",
)

var rootFSPath = flag.String(
	"rootfs",
	"",
	"directory of the rootfs for the containers",
)

var disableQuotas = flag.Bool(
	"disableQuotas",
	false,
	"disable disk quotas",
)

var containerGraceTime = flag.Duration(
	"containerGraceTime",
	0,
	"time after which to destroy idle containers",
)

var debug = flag.Bool(
	"debug",
	false,
	"show low-level command output",
)

var networkPool = flag.String(
	"networkPool",
	"10.254.0.0/22",
	"network pool CIDR for containers; each container will get a /30",
)

var portPoolStart = flag.Uint(
	"portPoolStart",
	61001,
	"start of ephemeral port range used for mapped container ports",
)

var portPoolSize = flag.Uint(
	"portPoolSize",
	5000,
	"size of port pool used for mapped container ports",
)

var uidPoolStart = flag.Uint(
	"uidPoolStart",
	10000,
	"start of per-container user ids",
)

var uidPoolSize = flag.Uint(
	"uidPoolSize",
	256,
	"size of the uid pool",
)

var denyNetworks = flag.String(
	"denyNetworks",
	"",
	"CIDR blocks representing IPs to blacklist",
)

var allowNetworks = flag.String(
	"allowNetworks",
	"",
	"CIDR blocks representing IPs to whitelist",
)

var graphRoot = flag.String(
	"graph",
	"/var/lib/warden-docker-graph",
	"docker image graph",
)

var dockerRegistry = flag.String(
	"registry",
	registry.IndexServerAddress(),
	"docker registry API endpoint",
)

var uniquenessTag = flag.String(
	"uniquenessTag",
	"",
	"server-wide identifier used for 'global' configuration",
)

func main() {
	flag.Parse()

	maxProcs := runtime.NumCPU()
	prevMaxProcs := runtime.GOMAXPROCS(maxProcs)

	log.Println("set GOMAXPROCS to", maxProcs, "was", prevMaxProcs)

	if *binPath == "" {
		log.Fatalln("must specify -bin with linux backend")
	}

	if *depotPath == "" {
		log.Fatalln("must specify -depot with linux backend")
	}

	if *overlaysPath == "" {
		log.Fatalln("must specify -overlays with linux backend")
	}

	if *rootFSPath == "" {
		log.Fatalln("must specify -rootfs with linux backend")
	}

	os.Setenv("UNIQUENESS_TAG", *uniquenessTag)

	uidPool := uid_pool.New(uint32(*uidPoolStart), uint32(*uidPoolSize))

	_, ipNet, err := net.ParseCIDR(*networkPool)
	if err != nil {
		log.Fatalln("error parsing CIDR:", err)
	}

	networkPool := network_pool.New(ipNet)

	// TODO: use /proc/sys/net/ipv4/ip_local_port_range by default (end + 1)
	portPool := port_pool.New(uint32(*portPoolStart), uint32(*portPoolSize))

	runner := &potato{*uniquenessTag, linux_command_runner.New(*debug)}

	quotaManager, err := quota_manager.New(*depotPath, *binPath, runner)
	if err != nil {
		log.Fatalln("error creating quota manager:", err)
	}

	if *disableQuotas {
		quotaManager.Disable()
	}

	if err := os.MkdirAll(*graphRoot, 0755); err != nil {
		log.Fatalln("error creating graph directory:", err)
	}

	graphDriver, err := graphdriver.New(*graphRoot, nil)
	if err != nil {
		log.Fatalln("error constructing graph driver:", err)
	}

	graph, err := graph.NewGraph(*graphRoot, graphDriver)
	if err != nil {
		log.Fatalln("error constructing graph:", err)
	}

	reg, err := registry.NewRegistry(nil, nil, *dockerRegistry, true)
	if err != nil {
		log.Fatalln(err)
	}

	repoFetcher := repository_fetcher.Retryable{repository_fetcher.New(reg, graph)}

	rootFSProviders := map[string]rootfs_provider.RootFSProvider{
		"":       rootfs_provider.NewOverlay(*binPath, *overlaysPath, *rootFSPath, runner),
		"docker": rootfs_provider.NewDocker(repoFetcher, graphDriver),
	}

	pool := container_pool.New(
		*binPath,
		*depotPath,
		rootFSProviders,
		uidPool,
		networkPool,
		portPool,
		strings.Split(*denyNetworks, ","),
		strings.Split(*allowNetworks, ","),
		runner,
		quotaManager,
	)

	systemInfo := system_info.NewProvider(*depotPath)

	backend := linux_backend.New(pool, systemInfo, *snapshotsPath)

	log.Println("setting up backend")

	err = backend.Setup()
	if err != nil {
		log.Fatalln("failed to set up backend:", err)
	}

	log.Println("starting server; listening with", *listenNetwork, "on", *listenAddr)

	graceTime := *containerGraceTime

	wardenServer := server.New(*listenNetwork, *listenAddr, graceTime, backend)

	err = wardenServer.Start()
	if err != nil {
		log.Fatalln("failed to start:", err)
	}

	signals := make(chan os.Signal, 1)

	go func() {
		<-signals
		log.Println("stopping...")
		wardenServer.Stop()
		os.Exit(0)
	}()

	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	select {}
}
