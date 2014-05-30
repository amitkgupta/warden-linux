package runner

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/cloudfoundry-incubator/garden/client"
	"github.com/cloudfoundry-incubator/garden/client/connection"
	"github.com/cloudfoundry-incubator/garden/warden"
	"github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

type Runner struct {
	Network string
	Addr    string

	DepotPath     string
	OverlaysPath  string
	BinPath       string
	RootFSPath    string
	SnapshotsPath string

	wardenBin     string
	wardenSession *gexec.Session

	tmpdir string
}

func New(wardenPath, binPath, rootFSPath, network, addr string) (*Runner, error) {
	runner := &Runner{
		Network:    network,
		Addr:       addr,
		BinPath:    binPath,
		RootFSPath: rootFSPath,

		wardenBin: wardenPath,
	}

	return runner, runner.Prepare()
}

func (r *Runner) Prepare() error {
	var err error

	r.tmpdir, err = ioutil.TempDir(os.TempDir(), "warden-linux-server")
	if err != nil {
		return err
	}

	r.DepotPath = filepath.Join(r.tmpdir, "containers")
	r.OverlaysPath = filepath.Join(r.tmpdir, "overlays")
	r.SnapshotsPath = filepath.Join(r.tmpdir, "snapshots")

	if err := os.Mkdir(r.DepotPath, 0755); err != nil {
		return err
	}

	if err := os.Mkdir(r.SnapshotsPath, 0755); err != nil {
		return err
	}

	return nil
}

func (r *Runner) Start(argv ...string) {
	wardenArgs := argv
	wardenArgs = append(
		wardenArgs,
		"--listenNetwork", r.Network,
		"--listenAddr", r.Addr,
		"--bin", r.BinPath,
		"--depot", r.DepotPath,
		"--overlays", r.OverlaysPath,
		"--rootfs", r.RootFSPath,
		"--snapshots", r.SnapshotsPath,
		"--debug",
		"--disableQuotas",
	)

	warden := exec.Command(r.wardenBin, wardenArgs...)

	warden.Stdout = os.Stdout
	warden.Stderr = os.Stderr

	session, err := gexec.Start(warden, ginkgo.GinkgoWriter, ginkgo.GinkgoWriter)
	Expect(err).ToNot(HaveOccurred())

	r.wardenSession = session

	err = r.WaitForStart()
	Expect(err).ToNot(HaveOccurred())
}

func (r *Runner) Stop() {
	if r.wardenSession == nil {
		return
	}

	r.wardenSession.Command.Process.Signal(os.Interrupt)
	Eventually(r.wardenSession.ExitCode, 10).ShouldNot(Equal(-1))

	r.wardenSession = nil
}

func (r *Runner) KillWithFire() {
	if r.wardenSession == nil {
		return
	}

	r.wardenSession.Command.Process.Kill()
	defer os.RemoveAll(r.tmpdir)
	Eventually(r.wardenSession.ExitCode, 10).ShouldNot(Equal(-1))
	r.wardenSession = nil
}

func (r *Runner) DestroyContainers() error {
	client := r.NewClient()

	containers, err := client.Containers(nil)
	if err != nil {
		return err
	}

	for _, container := range containers {
		err := client.Destroy(container.Handle())
		if err != nil {
			return err
		}
	}

	if err := os.RemoveAll(r.SnapshotsPath); err != nil {
		return err
	}

	return nil
}

func (r *Runner) NewClient() warden.Client {
	return client.New(&connection.Info{
		Network: r.Network,
		Addr:    r.Addr,
	})
}

func (r *Runner) WaitForStart() error {
	timeout := 10 * time.Second
	timeoutTimer := time.NewTimer(timeout)

	for {
		conn, dialErr := net.Dial(r.Network, r.Addr)

		if dialErr == nil {
			conn.Close()
			return nil
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeoutTimer.C:
			return fmt.Errorf("warden did not come up within %s", timeout)
		}
	}
}
