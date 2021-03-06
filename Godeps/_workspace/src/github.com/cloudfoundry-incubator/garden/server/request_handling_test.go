package server_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"

	"github.com/cloudfoundry-incubator/garden/client"
	"github.com/cloudfoundry-incubator/garden/client/connection"
	"github.com/cloudfoundry-incubator/garden/server"
	"github.com/cloudfoundry-incubator/garden/warden"
	"github.com/cloudfoundry-incubator/garden/warden/fakes"
)

var _ = Describe("When a client connects", func() {
	var socketPath string

	var serverBackend *fakes.FakeBackend

	var serverContainerGraceTime time.Duration

	var wardenServer *server.WardenServer
	var wardenClient warden.Client

	BeforeEach(func() {
		tmpdir, err := ioutil.TempDir(os.TempDir(), "warden-server-test")
		Ω(err).ShouldNot(HaveOccurred())

		socketPath = path.Join(tmpdir, "warden.sock")
		serverBackend = new(fakes.FakeBackend)
		serverContainerGraceTime = 42 * time.Second

		wardenServer = server.New(
			"unix",
			socketPath,
			serverContainerGraceTime,
			serverBackend,
		)

		err = wardenServer.Start()
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(ErrorDialing("unix", socketPath)).ShouldNot(HaveOccurred())

		wardenClient = client.New(connection.New("unix", socketPath))
	})

	Context("and the client sends a PingRequest", func() {
		Context("and the backend ping succeeds", func() {
			It("does not error", func() {
				Ω(wardenClient.Ping()).ShouldNot(HaveOccurred())
			})
		})

		Context("when the backend ping fails", func() {
			BeforeEach(func() {
				serverBackend.PingReturns(errors.New("oh no!"))
			})

			It("returns an error", func() {
				Ω(wardenClient.Ping()).Should(HaveOccurred())
			})
		})

		Context("when the server is not up", func() {
			BeforeEach(func() {
				wardenServer.Stop()
			})

			It("returns an error", func() {
				Ω(wardenClient.Ping()).Should(HaveOccurred())
			})
		})
	})

	Context("and the client sends a CapacityRequest", func() {
		BeforeEach(func() {
			serverBackend.CapacityReturns(warden.Capacity{
				MemoryInBytes: 1111,
				DiskInBytes:   2222,
				MaxContainers: 42,
			}, nil)
		})

		It("returns the backend's reported capacity", func() {
			capacity, err := wardenClient.Capacity()
			Ω(err).ShouldNot(HaveOccurred())

			Ω(capacity.MemoryInBytes).Should(Equal(uint64(1111)))
			Ω(capacity.DiskInBytes).Should(Equal(uint64(2222)))
			Ω(capacity.MaxContainers).Should(Equal(uint64(42)))
		})

		Context("when getting the capacity fails", func() {
			BeforeEach(func() {
				serverBackend.CapacityReturns(warden.Capacity{}, errors.New("oh no!"))
			})

			It("returns an error", func() {
				_, err := wardenClient.Capacity()
				Ω(err).Should(HaveOccurred())
			})
		})
	})

	Context("and the client sends a CreateRequest", func() {
		var fakeContainer *fakes.FakeContainer

		BeforeEach(func() {
			fakeContainer = new(fakes.FakeContainer)
			fakeContainer.HandleReturns("some-handle")

			serverBackend.CreateReturns(fakeContainer, nil)
		})

		It("returns a container with the created handle", func() {
			container, err := wardenClient.Create(warden.ContainerSpec{
				Handle: "some-handle",
			})
			Ω(err).ShouldNot(HaveOccurred())

			Ω(container.Handle()).Should(Equal("some-handle"))
		})

		It("creates the container with the spec from the request", func() {
			_, err := wardenClient.Create(warden.ContainerSpec{
				Handle:     "some-handle",
				GraceTime:  42 * time.Second,
				Network:    "some-network",
				RootFSPath: "/path/to/rootfs",
				BindMounts: []warden.BindMount{
					{
						SrcPath: "/bind/mount/src",
						DstPath: "/bind/mount/dst",
						Mode:    warden.BindMountModeRW,
						Origin:  warden.BindMountOriginContainer,
					},
				},
				Properties: warden.Properties{
					"prop-a": "val-a",
					"prop-b": "val-b",
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			Ω(serverBackend.CreateArgsForCall(0)).Should(Equal(warden.ContainerSpec{
				Handle:     "some-handle",
				GraceTime:  time.Duration(42 * time.Second),
				Network:    "some-network",
				RootFSPath: "/path/to/rootfs",
				BindMounts: []warden.BindMount{
					{
						SrcPath: "/bind/mount/src",
						DstPath: "/bind/mount/dst",
						Mode:    warden.BindMountModeRW,
						Origin:  warden.BindMountOriginContainer,
					},
				},
				Properties: map[string]string{
					"prop-a": "val-a",
					"prop-b": "val-b",
				},
			}))
		})

		Context("when a grace time is given", func() {
			It("destroys the container after it has been idle for the grace time", func() {
				graceTime := time.Second

				fakeContainer = new(fakes.FakeContainer)
				fakeContainer.HandleReturns("doomed-handle")

				serverBackend.GraceTimeReturns(graceTime)
				serverBackend.CreateReturns(fakeContainer, nil)
				serverBackend.LookupReturns(fakeContainer, nil)

				before := time.Now()

				_, err := wardenClient.Create(warden.ContainerSpec{})
				Ω(err).ShouldNot(HaveOccurred())

				Eventually(serverBackend.DestroyCallCount, 2*time.Second).Should(Equal(1))
				Ω(serverBackend.DestroyArgsForCall(0)).Should(Equal("doomed-handle"))

				Ω(time.Since(before)).Should(BeNumerically("~", graceTime, 100*time.Millisecond))
			})
		})

		Context("when a grace time is not given", func() {
			It("defaults it to the server's grace time", func() {
				_, err := wardenClient.Create(warden.ContainerSpec{
					Handle: "some-handle",
				})
				Ω(err).ShouldNot(HaveOccurred())

				spec := serverBackend.CreateArgsForCall(0)
				Ω(spec.GraceTime).Should(Equal(serverContainerGraceTime))
			})
		})

		Context("when creating the container fails", func() {
			BeforeEach(func() {
				serverBackend.CreateReturns(nil, errors.New("oh no!"))
			})

			It("returns an error", func() {
				_, err := wardenClient.Create(warden.ContainerSpec{
					Handle: "some-handle",
				})
				Ω(err).Should(HaveOccurred())
			})
		})
	})

	Context("and the client sends a destroy request", func() {
		It("destroys the container", func() {
			err := wardenClient.Destroy("some-handle")
			Ω(err).ShouldNot(HaveOccurred())

			Ω(serverBackend.DestroyArgsForCall(0)).Should(Equal("some-handle"))
		})

		Context("when destroying the container fails", func() {
			BeforeEach(func() {
				serverBackend.DestroyReturns(errors.New("oh no!"))
			})

			It("sends a WardenError response", func() {
				err := wardenClient.Destroy("some-handle")
				Ω(err).Should(HaveOccurred())
			})
		})
	})

	Context("and the client sends a ListRequest", func() {
		BeforeEach(func() {
			c1 := new(fakes.FakeContainer)
			c1.HandleReturns("some-handle")

			c2 := new(fakes.FakeContainer)
			c2.HandleReturns("another-handle")

			c3 := new(fakes.FakeContainer)
			c3.HandleReturns("super-handle")

			serverBackend.ContainersReturns([]warden.Container{c1, c2, c3}, nil)
		})

		It("returns the containers from the backend", func() {
			containers, err := wardenClient.Containers(nil)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(containers).Should(HaveLen(3))

			handles := make([]string, 3)
			for i, c := range containers {
				handles[i] = c.Handle()
			}

			Ω(handles).Should(ContainElement("some-handle"))
			Ω(handles).Should(ContainElement("another-handle"))
			Ω(handles).Should(ContainElement("super-handle"))
		})

		Context("when getting the containers fails", func() {
			BeforeEach(func() {
				serverBackend.ContainersReturns(nil, errors.New("oh no!"))
			})

			It("returns an error", func() {
				_, err := wardenClient.Containers(nil)
				Ω(err).Should(HaveOccurred())
			})
		})

		Context("and the client sends a ListRequest with a property filter", func() {
			It("forwards the filter to the backend", func() {
				_, err := wardenClient.Containers(warden.Properties{
					"foo": "bar",
				})
				Ω(err).ShouldNot(HaveOccurred())

				Ω(serverBackend.ContainersArgsForCall(serverBackend.ContainersCallCount() - 1)).Should(Equal(
					warden.Properties{
						"foo": "bar",
					},
				))
			})
		})
	})

	Context("when a container has been created", func() {
		var container warden.Container

		var fakeContainer *fakes.FakeContainer

		BeforeEach(func() {
			fakeContainer = new(fakes.FakeContainer)
			fakeContainer.HandleReturns("some-handle")

			serverBackend.CreateReturns(fakeContainer, nil)
			serverBackend.LookupReturns(fakeContainer, nil)
		})

		JustBeforeEach(func() {
			var err error

			container, err = wardenClient.Create(warden.ContainerSpec{})
			Ω(err).ShouldNot(HaveOccurred())
		})

		itResetsGraceTimeWhenHandling := func(call func()) {
			Context("when created with a grace time", func() {
				graceTime := 1 * time.Second

				BeforeEach(func() {
					serverBackend.GraceTimeReturns(graceTime)
				})

				It("resets the container's grace time", func() {
					for i := 0; i < 11; i++ {
						time.Sleep(graceTime / 10)
						call()
					}

					before := time.Now()

					Eventually(serverBackend.DestroyCallCount, 2*graceTime).Should(Equal(1))
					Ω(serverBackend.DestroyArgsForCall(0)).Should(Equal(container.Handle()))

					Ω(time.Since(before)).Should(BeNumerically("~", graceTime, 100*time.Millisecond))
				})
			})
		}

		itFailsWhenTheContainerIsNotFound := func(example func()) {
			Context("when the container is not found", func() {
				It("fails", func() {
					serverBackend.LookupReturns(nil, errors.New("not found"))
					example()
				})
			})
		}

		Describe("stopping", func() {
			It("stops the container and sends a StopResponse", func() {
				err := container.Stop(true)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.StopArgsForCall(0)).Should(Equal(true))
			})

			itFailsWhenTheContainerIsNotFound(func() {
				err := container.Stop(true)
				Ω(err).Should(HaveOccurred())
			})

			Context("when stopping the container fails", func() {
				BeforeEach(func() {
					fakeContainer.StopReturns(errors.New("oh no!"))
				})

				It("returns an error", func() {
					err := container.Stop(true)
					Ω(err).Should(HaveOccurred())
				})
			})

			itResetsGraceTimeWhenHandling(
				func() {
					err := container.Stop(false)
					Ω(err).ShouldNot(HaveOccurred())
				},
			)
		})

		Describe("streaming in", func() {
			It("streams the file in, waits for completion, and succeeds", func() {
				data := bytes.NewBufferString("chunk-1;chunk-2;chunk-3;")

				fakeContainer.StreamInStub = func(dest string, stream io.Reader) error {
					Ω(dest).Should(Equal("/dst/path"))
					Ω(ioutil.ReadAll(stream)).Should(Equal([]byte("chunk-1;chunk-2;chunk-3;")))
					return nil
				}

				err := container.StreamIn("/dst/path", data)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.StreamInCallCount()).Should(Equal(1))
			})

			itFailsWhenTheContainerIsNotFound(func() {
				err := container.StreamIn("/dst/path", nil)
				Ω(err).Should(HaveOccurred())
			})

			Context("when copying in to the container fails", func() {
				BeforeEach(func() {
					fakeContainer.StreamInReturns(errors.New("oh no!"))
				})

				It("fails", func() {
					err := container.StreamIn("/dst/path", nil)
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("streaming out", func() {
			JustBeforeEach(func() {
				fakeContainer.StreamOutReturns(
					ioutil.NopCloser(bytes.NewBuffer([]byte("hello-world!"))),
					nil,
				)
			})

			It("streams the bits out and succeeds", func() {
				reader, err := container.StreamOut("/src/path")
				Ω(err).ShouldNot(HaveOccurred())
				Ω(reader).ShouldNot(BeZero())

				streamedContent, err := ioutil.ReadAll(reader)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(string(streamedContent)).Should(Equal("hello-world!"))

				Ω(fakeContainer.StreamOutArgsForCall(0)).Should(Equal("/src/path"))
			})

			itResetsGraceTimeWhenHandling(func() {
				reader, err := container.StreamOut("/src/path")
				Ω(err).ShouldNot(HaveOccurred())
				Ω(reader).ShouldNot(BeZero())

				_, err = ioutil.ReadAll(reader)
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, err := container.StreamOut("/src/path")
				Ω(err).Should(HaveOccurred())
			})

			Context("when streaming out of the container fails", func() {
				JustBeforeEach(func() {
					fakeContainer.StreamOutReturns(nil, errors.New("oh no!"))
				})

				It("returns an error", func() {
					_, err := container.StreamOut("/src/path")
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("limiting bandwidth", func() {
			It("sets the container's bandwidth limits", func() {
				setLimits := warden.BandwidthLimits{
					RateInBytesPerSecond:      123,
					BurstRateInBytesPerSecond: 456,
				}

				err := container.LimitBandwidth(setLimits)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.LimitBandwidthArgsForCall(0)).Should(Equal(setLimits))
			})

			itResetsGraceTimeWhenHandling(func() {
				err := container.LimitBandwidth(warden.BandwidthLimits{
					RateInBytesPerSecond:      123,
					BurstRateInBytesPerSecond: 456,
				})
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				err := container.LimitBandwidth(warden.BandwidthLimits{
					RateInBytesPerSecond:      123,
					BurstRateInBytesPerSecond: 456,
				})
				Ω(err).Should(HaveOccurred())
			})

			Context("when limiting the bandwidth fails", func() {
				BeforeEach(func() {
					fakeContainer.LimitBandwidthReturns(errors.New("oh no!"))
				})

				It("fails", func() {
					err := container.LimitBandwidth(warden.BandwidthLimits{
						RateInBytesPerSecond:      123,
						BurstRateInBytesPerSecond: 456,
					})
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("getting the current bandwidth limits", func() {
			It("returns the limits returned by the backend", func() {
				effectiveLimits := warden.BandwidthLimits{
					RateInBytesPerSecond:      1230,
					BurstRateInBytesPerSecond: 4560,
				}

				fakeContainer.CurrentBandwidthLimitsReturns(effectiveLimits, nil)

				limits, err := container.CurrentBandwidthLimits()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(limits).Should(Equal(effectiveLimits))
			})

			Context("when getting the current limits fails", func() {
				BeforeEach(func() {
					fakeContainer.CurrentBandwidthLimitsReturns(warden.BandwidthLimits{}, errors.New("oh no!"))
				})

				It("fails", func() {
					_, err := container.CurrentBandwidthLimits()
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("limiting memory", func() {
			setLimits := warden.MemoryLimits{1024}

			It("sets the container's memory limits", func() {
				err := container.LimitMemory(setLimits)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.LimitMemoryArgsForCall(0)).Should(Equal(setLimits))
			})

			itResetsGraceTimeWhenHandling(func() {
				err := container.LimitMemory(setLimits)
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				err := container.LimitMemory(warden.MemoryLimits{123})
				Ω(err).Should(HaveOccurred())
			})

			Context("when limiting the memory fails", func() {
				BeforeEach(func() {
					fakeContainer.LimitMemoryReturns(errors.New("oh no!"))
				})

				It("fail", func() {
					err := container.LimitMemory(warden.MemoryLimits{123})
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("getting memory limits", func() {
			It("obtains the current limits", func() {
				effectiveLimits := warden.MemoryLimits{2048}
				fakeContainer.CurrentMemoryLimitsReturns(effectiveLimits, nil)

				limits, err := container.CurrentMemoryLimits()
				Ω(err).ShouldNot(HaveOccurred())
				Ω(limits).ShouldNot(BeZero())

				Ω(limits).Should(Equal(effectiveLimits))
			})

			It("does not change the memory limit", func() {
				_, err := container.CurrentMemoryLimits()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.LimitMemoryCallCount()).Should(BeZero())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, err := container.CurrentMemoryLimits()
				Ω(err).Should(HaveOccurred())
			})

			Context("when getting the current memory limits fails", func() {
				BeforeEach(func() {
					fakeContainer.CurrentMemoryLimitsReturns(warden.MemoryLimits{}, errors.New("oh no!"))
				})

				It("fails", func() {
					_, err := container.CurrentMemoryLimits()
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("limiting disk", func() {
			setLimits := warden.DiskLimits{
				BlockSoft: 111,
				BlockHard: 222,

				InodeSoft: 333,
				InodeHard: 444,

				ByteSoft: 555,
				ByteHard: 666,
			}

			It("sets the container's disk limits and returns the current limits", func() {
				err := container.LimitDisk(setLimits)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.LimitDiskArgsForCall(0)).Should(Equal(setLimits))
			})

			itResetsGraceTimeWhenHandling(func() {
				err := container.LimitDisk(setLimits)
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				err := container.LimitDisk(warden.DiskLimits{})
				Ω(err).Should(HaveOccurred())
			})

			Context("when limiting the disk fails", func() {
				BeforeEach(func() {
					fakeContainer.LimitDiskReturns(errors.New("oh no!"))
				})

				It("fails", func() {
					err := container.LimitDisk(warden.DiskLimits{})
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("getting the current disk limits", func() {
			currentLimits := warden.DiskLimits{
				BlockSoft: 1111,
				BlockHard: 2222,

				InodeSoft: 3333,
				InodeHard: 4444,

				ByteSoft: 5555,
				ByteHard: 6666,
			}

			It("returns the limits returned by the backend", func() {
				fakeContainer.CurrentDiskLimitsReturns(currentLimits, nil)

				limits, err := container.CurrentDiskLimits()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(limits).Should(Equal(currentLimits))
			})

			It("does not change the disk limit", func() {
				_, err := container.CurrentDiskLimits()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.LimitDiskCallCount()).Should(BeZero())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, err := container.CurrentDiskLimits()
				Ω(err).Should(HaveOccurred())
			})

			Context("when getting the current disk limits fails", func() {
				BeforeEach(func() {
					fakeContainer.CurrentDiskLimitsReturns(warden.DiskLimits{}, errors.New("oh no!"))
				})

				It("fails", func() {
					_, err := container.CurrentDiskLimits()
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("set the cpu limit", func() {
			setLimits := warden.CPULimits{123}

			It("sets the container's CPU shares", func() {
				err := container.LimitCPU(setLimits)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.LimitCPUArgsForCall(0)).Should(Equal(setLimits))
			})

			itResetsGraceTimeWhenHandling(func() {
				err := container.LimitCPU(setLimits)
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				err := container.LimitCPU(setLimits)
				Ω(err).Should(HaveOccurred())
			})

			Context("when limiting the CPU fails", func() {
				BeforeEach(func() {
					fakeContainer.LimitCPUReturns(errors.New("oh no!"))
				})

				It("fails", func() {
					err := container.LimitCPU(setLimits)
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("get the current cpu limits", func() {
			effectiveLimits := warden.CPULimits{456}

			It("gets the current limits", func() {
				fakeContainer.CurrentCPULimitsReturns(effectiveLimits, nil)

				limits, err := container.CurrentCPULimits()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(limits).Should(Equal(effectiveLimits))
			})

			It("does not change the cpu limits", func() {
				_, err := container.CurrentCPULimits()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeContainer.LimitCPUCallCount()).Should(BeZero())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, err := container.CurrentCPULimits()
				Ω(err).Should(HaveOccurred())
			})

			Context("when getting the current CPU limits fails", func() {
				BeforeEach(func() {
					fakeContainer.CurrentCPULimitsReturns(warden.CPULimits{}, errors.New("oh no!"))
				})

				It("fails", func() {
					_, err := container.CurrentCPULimits()
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("net in", func() {
			It("maps the ports and returns them", func() {
				fakeContainer.NetInReturns(111, 222, nil)

				hostPort, containerPort, err := container.NetIn(123, 456)
				Ω(err).ShouldNot(HaveOccurred())

				hp, cp := fakeContainer.NetInArgsForCall(0)
				Ω(hp).Should(Equal(uint32(123)))
				Ω(cp).Should(Equal(uint32(456)))

				Ω(hostPort).Should(Equal(uint32(111)))
				Ω(containerPort).Should(Equal(uint32(222)))
			})

			itResetsGraceTimeWhenHandling(func() {
				_, _, err := container.NetIn(123, 456)
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, _, err := container.NetIn(123, 456)
				Ω(err).Should(HaveOccurred())
			})

			Context("when mapping the port fails", func() {
				BeforeEach(func() {
					fakeContainer.NetInReturns(0, 0, errors.New("oh no!"))
				})

				It("fails", func() {
					_, _, err := container.NetIn(123, 456)
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("net out", func() {
			It("permits traffic outside of the container", func() {
				err := container.NetOut("1.2.3.4/22", 456)
				Ω(err).ShouldNot(HaveOccurred())

				cidr, port := fakeContainer.NetOutArgsForCall(0)
				Ω(cidr).Should(Equal("1.2.3.4/22"))
				Ω(port).Should(Equal(uint32(456)))
			})

			itResetsGraceTimeWhenHandling(func() {
				err := container.NetOut("1.2.3.4/22", 456)
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				err := container.NetOut("1.2.3.4/22", 456)
				Ω(err).Should(HaveOccurred())
			})

			Context("when permitting traffic fails", func() {
				BeforeEach(func() {
					fakeContainer.NetOutReturns(errors.New("oh no!"))
				})

				It("fails", func() {
					err := container.NetOut("1.2.3.4/22", 456)
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("info", func() {
			containerInfo := warden.ContainerInfo{
				State:         "active",
				Events:        []string{"oom", "party"},
				HostIP:        "host-ip",
				ContainerIP:   "container-ip",
				ContainerPath: "/path/to/container",
				ProcessIDs:    []uint32{1, 2},
				Properties: warden.Properties{
					"foo": "bar",
					"a":   "b",
				},
				MemoryStat: warden.ContainerMemoryStat{
					Cache:                   1,
					Rss:                     2,
					MappedFile:              3,
					Pgpgin:                  4,
					Pgpgout:                 5,
					Swap:                    6,
					Pgfault:                 7,
					Pgmajfault:              8,
					InactiveAnon:            9,
					ActiveAnon:              10,
					InactiveFile:            11,
					ActiveFile:              12,
					Unevictable:             13,
					HierarchicalMemoryLimit: 14,
					HierarchicalMemswLimit:  15,
					TotalCache:              16,
					TotalRss:                17,
					TotalMappedFile:         18,
					TotalPgpgin:             19,
					TotalPgpgout:            20,
					TotalSwap:               21,
					TotalPgfault:            22,
					TotalPgmajfault:         23,
					TotalInactiveAnon:       24,
					TotalActiveAnon:         25,
					TotalInactiveFile:       26,
					TotalActiveFile:         27,
					TotalUnevictable:        28,
				},
				CPUStat: warden.ContainerCPUStat{
					Usage:  1,
					User:   2,
					System: 3,
				},
				DiskStat: warden.ContainerDiskStat{
					BytesUsed:  1,
					InodesUsed: 2,
				},
				BandwidthStat: warden.ContainerBandwidthStat{
					InRate:   1,
					InBurst:  2,
					OutRate:  3,
					OutBurst: 4,
				},
				MappedPorts: []warden.PortMapping{
					{HostPort: 1234, ContainerPort: 5678},
					{HostPort: 1235, ContainerPort: 5679},
				},
			}

			It("reports information about the container", func() {
				fakeContainer.InfoReturns(containerInfo, nil)

				info, err := container.Info()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(info).Should(Equal(containerInfo))
			})

			itResetsGraceTimeWhenHandling(func() {
				_, err := container.Info()
				Ω(err).ShouldNot(HaveOccurred())
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, err := container.Info()
				Ω(err).Should(HaveOccurred())
			})

			Context("when getting container info fails", func() {
				BeforeEach(func() {
					fakeContainer.InfoReturns(warden.ContainerInfo{}, errors.New("oh no!"))
				})

				It("fails", func() {
					_, err := container.Info()
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("attaching", func() {
			Context("when attaching succeeds", func() {
				BeforeEach(func() {
					fakeContainer.AttachStub = func(processID uint32, io warden.ProcessIO) (warden.Process, error) {
						writing := new(sync.WaitGroup)
						writing.Add(1)

						go func() {
							defer writing.Done()
							defer GinkgoRecover()

							_, err := fmt.Fprintf(io.Stdout, "stdout data")
							Ω(err).ShouldNot(HaveOccurred())

							in, err := ioutil.ReadAll(io.Stdin)
							Ω(err).ShouldNot(HaveOccurred())

							_, err = fmt.Fprintf(io.Stdout, "mirrored %s", string(in))
							Ω(err).ShouldNot(HaveOccurred())

							_, err = fmt.Fprintf(io.Stderr, "stderr data")
							Ω(err).ShouldNot(HaveOccurred())
						}()

						process := new(fakes.FakeProcess)

						process.IDReturns(42)

						process.WaitStub = func() (int, error) {
							writing.Wait()
							return 123, nil
						}

						return process, nil
					}
				})

				It("responds with a ProcessPayload for every chunk", func() {
					stdout := gbytes.NewBuffer()
					stderr := gbytes.NewBuffer()

					processIO := warden.ProcessIO{
						Stdin:  bytes.NewBufferString("stdin data"),
						Stdout: stdout,
						Stderr: stderr,
					}

					process, err := container.Attach(42, processIO)
					Ω(err).ShouldNot(HaveOccurred())

					pid, _ := fakeContainer.AttachArgsForCall(0)
					Ω(pid).Should(Equal(uint32(42)))

					Eventually(stdout).Should(gbytes.Say("stdout data"))
					Eventually(stdout).Should(gbytes.Say("mirrored stdin data"))
					Eventually(stderr).Should(gbytes.Say("stderr data"))

					status, err := process.Wait()
					Ω(err).ShouldNot(HaveOccurred())
					Ω(status).Should(Equal(123))
				})

				itResetsGraceTimeWhenHandling(func() {
					process, err := container.Attach(42, warden.ProcessIO{
						Stdin: bytes.NewBufferString("hello"),
					})
					Ω(err).ShouldNot(HaveOccurred())

					status, err := process.Wait()
					Ω(err).ShouldNot(HaveOccurred())
					Ω(status).Should(Equal(123))
				})
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, err := container.Attach(123, warden.ProcessIO{})
				Ω(err).Should(HaveOccurred())
			})

			Context("when waiting on the process fails server-side", func() {
				BeforeEach(func() {
					fakeContainer.AttachStub = func(id uint32, io warden.ProcessIO) (warden.Process, error) {
						process := new(fakes.FakeProcess)

						process.IDReturns(42)
						process.WaitReturns(0, errors.New("oh no!"))

						return process, nil
					}
				})

				It("bubbles the error up", func() {
					process, err := container.Attach(42, warden.ProcessIO{})
					Ω(err).ShouldNot(HaveOccurred())

					_, err = process.Wait()
					Ω(err).Should(HaveOccurred())
					Ω(err.Error()).Should(ContainSubstring("oh no!"))
				})
			})

			Context("when attaching fails", func() {
				BeforeEach(func() {
					fakeContainer.AttachReturns(nil, errors.New("oh no!"))
				})

				It("fails", func() {
					_, err := container.Attach(123, warden.ProcessIO{})
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Describe("running", func() {
			processSpec := warden.ProcessSpec{
				Path: "/some/script",
				Args: []string{"arg1", "arg2"},
				Dir:  "/some/dir",
				Env: []string{
					"FLAVOR=chocolate",
					"TOPPINGS=sprinkles",
				},
				Privileged: true,
				Limits: warden.ResourceLimits{
					As:         uint64ptr(1),
					Core:       uint64ptr(2),
					Cpu:        uint64ptr(3),
					Data:       uint64ptr(4),
					Fsize:      uint64ptr(5),
					Locks:      uint64ptr(6),
					Memlock:    uint64ptr(7),
					Msgqueue:   uint64ptr(8),
					Nice:       uint64ptr(9),
					Nofile:     uint64ptr(10),
					Nproc:      uint64ptr(11),
					Rss:        uint64ptr(12),
					Rtprio:     uint64ptr(13),
					Sigpending: uint64ptr(14),
					Stack:      uint64ptr(15),
				},
				TTY: &warden.TTYSpec{
					WindowSize: &warden.WindowSize{
						Columns: 80,
						Rows:    24,
					},
				},
			}

			Context("when running succeeds", func() {
				BeforeEach(func() {
					fakeContainer.RunStub = func(spec warden.ProcessSpec, io warden.ProcessIO) (warden.Process, error) {
						writing := new(sync.WaitGroup)
						writing.Add(1)

						go func() {
							defer writing.Done()
							defer GinkgoRecover()

							_, err := fmt.Fprintf(io.Stdout, "stdout data")
							Ω(err).ShouldNot(HaveOccurred())

							in, err := ioutil.ReadAll(io.Stdin)
							Ω(err).ShouldNot(HaveOccurred())

							_, err = fmt.Fprintf(io.Stdout, "mirrored %s", string(in))
							Ω(err).ShouldNot(HaveOccurred())

							_, err = fmt.Fprintf(io.Stderr, "stderr data")
							Ω(err).ShouldNot(HaveOccurred())
						}()

						process := new(fakes.FakeProcess)

						process.IDReturns(42)

						process.WaitStub = func() (int, error) {
							writing.Wait()
							return 123, nil
						}

						return process, nil
					}
				})

				It("runs the process and streams the output", func() {
					stdout := gbytes.NewBuffer()
					stderr := gbytes.NewBuffer()

					processIO := warden.ProcessIO{
						Stdin:  bytes.NewBufferString("stdin data"),
						Stdout: stdout,
						Stderr: stderr,
					}

					process, err := container.Run(processSpec, processIO)
					Ω(err).ShouldNot(HaveOccurred())

					ranSpec, _ := fakeContainer.RunArgsForCall(0)
					Ω(ranSpec).Should(Equal(processSpec))

					Eventually(stdout).Should(gbytes.Say("stdout data"))
					Eventually(stdout).Should(gbytes.Say("mirrored stdin data"))
					Eventually(stderr).Should(gbytes.Say("stderr data"))

					status, err := process.Wait()
					Ω(err).ShouldNot(HaveOccurred())
					Ω(status).Should(Equal(123))
				})

				itResetsGraceTimeWhenHandling(func() {
					process, err := container.Run(processSpec, warden.ProcessIO{
						Stdin: bytes.NewBufferString("hello"),
					})
					Ω(err).ShouldNot(HaveOccurred())

					status, err := process.Wait()
					Ω(err).ShouldNot(HaveOccurred())
					Ω(status).Should(Equal(123))
				})
			})

			Context("when the process's window size is set", func() {
				var fakeProcess *fakes.FakeProcess

				BeforeEach(func() {
					fakeProcess = new(fakes.FakeProcess)
					fakeProcess.IDReturns(42)
					fakeProcess.WaitStub = func() (int, error) {
						select {}
						return 0, nil
					}

					fakeContainer.RunReturns(fakeProcess, nil)
				})

				It("is eventually set in the backend", func() {
					process, err := container.Run(processSpec, warden.ProcessIO{})
					Ω(err).ShouldNot(HaveOccurred())

					ttySpec := warden.TTYSpec{
						WindowSize: &warden.WindowSize{
							Columns: 80,
							Rows:    24,
						},
					}

					err = process.SetTTY(ttySpec)
					Ω(err).ShouldNot(HaveOccurred())

					Eventually(fakeProcess.SetTTYCallCount).Should(Equal(1))

					Ω(fakeProcess.SetTTYArgsForCall(0)).Should(Equal(ttySpec))
				})
			})

			Context("when waiting on the process fails server-side", func() {
				BeforeEach(func() {
					fakeContainer.RunStub = func(spec warden.ProcessSpec, io warden.ProcessIO) (warden.Process, error) {
						process := new(fakes.FakeProcess)

						process.IDReturns(42)
						process.WaitReturns(0, errors.New("oh no!"))

						return process, nil
					}
				})

				It("bubbles the error up", func() {
					process, err := container.Run(processSpec, warden.ProcessIO{})
					Ω(err).ShouldNot(HaveOccurred())

					_, err = process.Wait()
					Ω(err).Should(HaveOccurred())
					Ω(err.Error()).Should(ContainSubstring("oh no!"))
				})
			})

			itFailsWhenTheContainerIsNotFound(func() {
				_, err := container.Run(processSpec, warden.ProcessIO{})
				Ω(err).Should(HaveOccurred())
			})

			Context("when running fails", func() {
				BeforeEach(func() {
					fakeContainer.RunReturns(nil, errors.New("oh no!"))
				})

				It("fails", func() {
					_, err := container.Run(processSpec, warden.ProcessIO{})
					Ω(err).Should(HaveOccurred())
				})
			})
		})
	})
})
