// +build linux

package wshd_test

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
	. "github.com/onsi/gomega/gexec"
)

var _ = Describe("Running wshd", func() {
	wshd := "../../linux_backend/skeleton/bin/wshd"

	wsh := "../../linux_backend/skeleton/bin/wsh"

	shmTest, err := Build("github.com/cloudfoundry-incubator/warden-linux/integration/wshd/shm_test")
	if err != nil {
		panic(err)
	}

	var socketPath string
	var containerPath string

	var binDir string
	var libDir string
	var runDir string
	var mntDir string

	BeforeEach(func() {
		var err error

		containerPath, err = ioutil.TempDir(os.TempDir(), "wshd-test-container")
		Ω(err).ShouldNot(HaveOccurred())

		binDir = path.Join(containerPath, "bin")
		libDir = path.Join(containerPath, "lib")
		runDir = path.Join(containerPath, "run")
		mntDir = path.Join(containerPath, "mnt")

		os.Mkdir(binDir, 0755)
		os.Mkdir(libDir, 0755)
		os.Mkdir(runDir, 0755)

		err = copyFile(wshd, path.Join(binDir, "wshd"))
		Ω(err).ShouldNot(HaveOccurred())

		ioutil.WriteFile(path.Join(libDir, "hook-parent-before-clone.sh"), []byte(`#!/bin/bash

set -o nounset
set -o errexit
shopt -s nullglob

cd $(dirname $0)/../

cp bin/wshd mnt/sbin/wshd
chmod 700 mnt/sbin/wshd
`), 0755)

		ioutil.WriteFile(path.Join(libDir, "hook-parent-after-clone.sh"), []byte(`#!/bin/bash
set -o nounset
set -o errexit
shopt -s nullglob

cd $(dirname $0)/../

cat > /proc/$PID/uid_map <<EOF
0 0 1
EOF

cat > /proc/$PID/gid_map <<EOF
0 0 1
EOF

echo $PID > ./run/wshd.pid
`), 0755)

		ioutil.WriteFile(path.Join(libDir, "hook-child-before-pivot.sh"), []byte(`#!/bin/bash
`), 0755)

		ioutil.WriteFile(path.Join(libDir, "hook-child-after-pivot.sh"), []byte(`#!/bin/bash

set -o nounset
set -o errexit
shopt -s nullglob

cd $(dirname $0)/../

mkdir -p /proc
mount -t proc none /proc
`), 0755)

		ioutil.WriteFile(path.Join(libDir, "set-up-root.sh"), []byte(`#!/bin/bash

set -o nounset
set -o errexit
shopt -s nullglob

rootfs_path=$1

function overlay_directory_in_rootfs() {
  # Skip if exists
  if [ ! -d tmp/rootfs/$1 ]
  then
    if [ -d mnt/$1 ]
    then
      cp -r mnt/$1 tmp/rootfs/
    else
      mkdir -p tmp/rootfs/$1
    fi
  fi

  mount -n --bind tmp/rootfs/$1 mnt/$1
  mount -n --bind -o remount,$2 tmp/rootfs/$1 mnt/$1
}

function setup_fs() {
  mkdir -p tmp/rootfs mnt

  mkdir -p $rootfs_path/proc

  mount -n --bind $rootfs_path mnt
  mount -n --bind -o remount,ro $rootfs_path mnt

  overlay_directory_in_rootfs /dev rw
  overlay_directory_in_rootfs /etc rw
  overlay_directory_in_rootfs /home rw
  overlay_directory_in_rootfs /sbin rw
  overlay_directory_in_rootfs /var rw

  mkdir -p tmp/rootfs/tmp
  chmod 777 tmp/rootfs/tmp
  overlay_directory_in_rootfs /tmp rw
}

setup_fs
`), 0755)

		setUpRoot := exec.Command(path.Join(libDir, "set-up-root.sh"), os.Getenv("WARDEN_TEST_ROOTFS"))
		setUpRoot.Dir = containerPath

		setUpRootSession, err := Start(setUpRoot, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(setUpRootSession, 5.0).Should(Exit(0))
	})

	JustBeforeEach(func() {
		wshdCommand := exec.Command(
			wshd,
			"--run", runDir,
			"--lib", libDir,
			"--root", mntDir,
			"--title", "test wshd",
		)

		socketPath = path.Join(runDir, "wshd.sock")

		wshdSession, err := Start(wshdCommand, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(wshdSession, 30).Should(Exit(0))

		Eventually(ErrorDialingUnix(socketPath)).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		wshdPidfile, err := os.Open(path.Join(containerPath, "run", "wshd.pid"))
		Ω(err).ShouldNot(HaveOccurred())

		var wshdPid int
		_, err = fmt.Fscanf(wshdPidfile, "%d", &wshdPid)
		Ω(err).ShouldNot(HaveOccurred())

		proc, err := os.FindProcess(wshdPid)
		Ω(err).ShouldNot(HaveOccurred())

		err = proc.Kill()
		Ω(err).ShouldNot(HaveOccurred())

		for _, submount := range []string{"dev", "etc", "home", "sbin", "var", "tmp"} {
			mountPoint := path.Join(containerPath, "mnt", submount)

			err := syscall.Unmount(mountPoint, 0)
			Ω(err).ShouldNot(HaveOccurred())
		}

		err = syscall.Unmount(path.Join(containerPath, "mnt"), 0)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(func() error {
			return os.RemoveAll(containerPath)
		}, 10).ShouldNot(HaveOccurred())
	})

	It("starts the daemon as a session leader with process isolation and the given title", func() {
		ps := exec.Command(wsh, "--socket", socketPath, "/bin/ps", "-o", "pid,command")

		psSession, err := Start(ps, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(psSession).Should(Say(`1\s+test wshd`))
		Eventually(psSession).Should(Exit(0))
	})

	It("starts the daemon with mount space isolation", func() {
		mkdir := exec.Command(wsh, "--socket", socketPath, "/bin/mkdir", "/tmp/lawn")
		mkdirSession, err := Start(mkdir, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(mkdirSession).Should(Exit(0))

		mkdir = exec.Command(wsh, "--socket", socketPath, "/bin/mkdir", "/tmp/gnome")
		mkdirSession, err = Start(mkdir, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(mkdirSession).Should(Exit(0))

		mount := exec.Command(wsh, "--socket", socketPath, "/bin/mount", "--bind", "/tmp/lawn", "/tmp/gnome")
		mountSession, err := Start(mount, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(mountSession).Should(Exit(0))

		cat := exec.Command("/bin/cat", "/proc/mounts")
		catSession, err := Start(cat, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Ω(catSession).ShouldNot(Say("gnome"))
		Eventually(catSession).Should(Exit(0))
	})

	It("places the daemon in each cgroup subsystem", func() {
		cat := exec.Command(wsh, "--socket", socketPath, "bash", "-c", "cat /proc/$$/cgroup")
		catSession, err := Start(cat, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(catSession).Should(Exit(0))
		Ω(catSession.Out.Contents()).Should(MatchRegexp(`\bcpu\b`))
		Ω(catSession.Out.Contents()).Should(MatchRegexp(`\bcpuacct\b`))
		Ω(catSession.Out.Contents()).Should(MatchRegexp(`\bcpuset\b`))
		Ω(catSession.Out.Contents()).Should(MatchRegexp(`\bdevices\b`))
		Ω(catSession.Out.Contents()).Should(MatchRegexp(`\bmemory\b`))
	})

	It("starts the daemon with network namespace isolation", func() {
		ifconfig := exec.Command(wsh, "--socket", socketPath, "/sbin/ifconfig", "lo:0", "1.2.3.4", "up")
		ifconfigSession, err := Start(ifconfig, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(ifconfigSession).Should(Exit(0))

		localIfconfig := exec.Command("ifconfig")
		localIfconfigSession, err := Start(localIfconfig, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Ω(localIfconfigSession).ShouldNot(Say("lo:0"))
		Eventually(localIfconfigSession).Should(Exit(0))
	})

	It("starts the daemon with a new IPC namespace", func() {
		err = copyFile(shmTest, path.Join(mntDir, "sbin", "shmtest"))
		Ω(err).ShouldNot(HaveOccurred())

		localSHM := exec.Command(shmTest)
		createLocal, err := Start(
			localSHM,
			GinkgoWriter,
			GinkgoWriter,
		)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(createLocal).Should(Say("ok"))

		createRemote, err := Start(
			exec.Command(wsh, "--socket", socketPath, "/sbin/shmtest", "create"),
			GinkgoWriter,
			GinkgoWriter,
		)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(createRemote).Should(Say("ok"))

		localSHM.Process.Signal(syscall.SIGUSR2)

		Eventually(createLocal).Should(Exit(0))
	})

	It("starts the daemon with a new UTS namespace", func() {
		hostname := exec.Command(wsh, "--socket", socketPath, "/bin/hostname", "newhostname")
		hostnameSession, err := Start(hostname, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(hostnameSession).Should(Exit(0))

		localHostname := exec.Command("hostname")
		localHostnameSession, err := Start(localHostname, GinkgoWriter, GinkgoWriter)
		Ω(localHostnameSession).ShouldNot(Say("newhostname"))
	})

	It("does not leak any shared memory to the child", func() {
		createRemote, err := Start(
			exec.Command(wsh, "--socket", socketPath, "ipcs"),
			GinkgoWriter,
			GinkgoWriter,
		)
		Ω(err).ShouldNot(HaveOccurred())
		Ω(createRemote).ShouldNot(Say("deadbeef"))
	})

	It("unmounts /tmp/warden-host* in the child", func() {
		cat := exec.Command(wsh, "--socket", socketPath, "/bin/cat", "/proc/mounts")

		catSession, err := Start(cat, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(catSession).ShouldNot(Say(" /tmp/warden-host"))
		Eventually(catSession).Should(Exit(0))
	})

	It("sets the specified environment variables", func() {
		pwd := exec.Command(wsh,
			"--socket", socketPath,
			"--env", "VAR1=VALUE1",
			"--env", "VAR2=VALUE2",
			"bash", "-c", "env | sort",
		)

		session, err := Start(pwd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(session).Should(Say("VAR1=VALUE1\n"))
		Eventually(session).Should(Say("VAR2=VALUE2\n"))
	})

	Context("when mount points on the host are deleted", func() {
		BeforeEach(func() {
			tmpdir, err := ioutil.TempDir("", "wshd-bogus-mount")
			Ω(err).ShouldNot(HaveOccurred())

			fooDir := filepath.Join(tmpdir, "foo")
			barDir := filepath.Join(tmpdir, "bar")

			err = os.MkdirAll(fooDir, 0755)
			Ω(err).ShouldNot(HaveOccurred())

			err = os.MkdirAll(barDir, 0755)
			Ω(err).ShouldNot(HaveOccurred())

			mount := exec.Command("mount", "--bind", fooDir, barDir)
			mountSession, err := Start(mount, GinkgoWriter, GinkgoWriter)
			Ω(err).ShouldNot(HaveOccurred())
			Eventually(mountSession).Should(Exit(0))

			err = os.RemoveAll(fooDir)
			Ω(err).ShouldNot(HaveOccurred())

			cat := exec.Command("/bin/cat", "/proc/mounts")
			catSession, err := Start(cat, GinkgoWriter, GinkgoWriter)
			Ω(err).ShouldNot(HaveOccurred())
			Eventually(catSession).Should(Say("(deleted)"))
			Eventually(catSession).Should(Exit(0))
		})

		It("unmounts the un-mangled mount point name", func() {
			cat := exec.Command(wsh, "--socket", socketPath, "/bin/cat", "/proc/mounts")

			catSession, err := Start(cat, GinkgoWriter, GinkgoWriter)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(catSession).ShouldNot(Say("(deleted)"))
			Eventually(catSession).Should(Exit(0))
		})
	})

	Context("when running a command in a working dir", func() {
		It("executes with setuid and setgid", func() {
			bash := exec.Command(wsh, "--socket", socketPath, "--dir", "/usr", "pwd")

			bashSession, err := Start(bash, GinkgoWriter, GinkgoWriter)
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(bashSession).Should(Say("^/usr\n"))
			Eventually(bashSession).Should(Exit(0))
		})
	})

	It("executes with setuid and setgid 0", func() {
		bash := exec.Command(wsh, "--socket", socketPath, "--user", "root", "/bin/bash", "-c", "id -u; id -g")

		bashSession, err := Start(bash, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(bashSession).Should(Say("^0\n"))
		Eventually(bashSession).Should(Say("^0\n"))
		Eventually(bashSession).Should(Exit(0))
	})

	It("sets $HOME, $USER, and a $PATH with sbin dirs", func() {
		bash := exec.Command(wsh, "--socket", socketPath, "--user", "root", "/bin/bash", "-c", "env | sort")

		bashSession, err := Start(bash, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(bashSession).Should(Say("HOME=/root\n"))
		Eventually(bashSession).Should(Say("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n"))
		Eventually(bashSession).Should(Say("USER=root\n"))
		Eventually(bashSession).Should(Exit(0))
	})

	It("executes in root's home directory", func() {
		pwd := exec.Command(wsh, "--socket", socketPath, "--user", "root", "/bin/pwd")

		pwdSession, err := Start(pwd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(pwdSession).Should(Say("/root\n"))
		Eventually(pwdSession).Should(Exit(0))
	})

	Context("when piping stdin", func() {
		It("terminates when the input stream terminates", func() {
			bash := exec.Command(wsh, "--socket", socketPath, "/bin/bash")

			stdin, err := bash.StdinPipe()
			Ω(err).ShouldNot(HaveOccurred())

			bashSession, err := Start(bash, GinkgoWriter, GinkgoWriter)
			Ω(err).ShouldNot(HaveOccurred())

			stdin.Write([]byte("echo hello"))
			stdin.Close()

			Eventually(bashSession).Should(Say("hello\n"))
			Eventually(bashSession).Should(Exit(0))
		})
	})
})

func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}

	defer s.Close()

	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE, 0755)
	if err != nil {
		return err
	}

	_, err = io.Copy(d, s)
	if err != nil {
		d.Close()
		return err
	}

	return d.Close()
}

func ErrorDialingUnix(socketPath string) func() error {
	return func() error {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
		}

		return err
	}
}
