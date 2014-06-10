package lifecycle_test

import (
	"fmt"
	"strings"

	"github.com/cloudfoundry-incubator/garden/warden"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Placing limits on containers", func() {
	Describe("denying access to network ranges", func() {
		var (
			blockedListener   warden.Container
			blockedListenerIP string

			unblockedListener   warden.Container
			unblockedListenerIP string

			allowedListener   warden.Container
			allowedListenerIP string

			sender warden.Container
		)

		BeforeEach(func() {
			var err error

			// create a listener to which we deny network access
			blockedListener, err = client.Create(warden.ContainerSpec{})
			Expect(err).ToNot(HaveOccurred())
			info, err := blockedListener.Info()
			Expect(err).ToNot(HaveOccurred())
			blockedListenerIP = info.ContainerIP

			// create a listener to which we do not deny access
			unblockedListener, err = client.Create(warden.ContainerSpec{})
			Expect(err).ToNot(HaveOccurred())
			info, err = unblockedListener.Info()
			Expect(err).ToNot(HaveOccurred())
			unblockedListenerIP = info.ContainerIP

			// create a listener to which we exclicitly allow access
			allowedListener, err = client.Create(warden.ContainerSpec{})
			Expect(err).ToNot(HaveOccurred())
			info, err = allowedListener.Info()
			Expect(err).ToNot(HaveOccurred())
			allowedListenerIP = info.ContainerIP

			runner.Stop()
			runner.Start(
				"-denyNetworks", strings.Join([]string{
					blockedListenerIP + "/32",
					allowedListenerIP + "/32",
				}, ","),
				"-allowNetworks", allowedListenerIP+"/32",
			)

			// create a container with the new deny network configuration
			sender, err = client.Create(warden.ContainerSpec{})
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			err := client.Destroy(sender.Handle())
			Expect(err).ToNot(HaveOccurred())

			err = client.Destroy(blockedListener.Handle())
			Expect(err).ToNot(HaveOccurred())

			err = client.Destroy(unblockedListener.Handle())
			Expect(err).ToNot(HaveOccurred())

			err = client.Destroy(allowedListener.Handle())
			Expect(err).ToNot(HaveOccurred())
		})

		expectStreamToExitWith := func(stream <-chan warden.ProcessStream, status int) {
			for chunk := range stream {
				if chunk.ExitStatus != nil {
					ExpectWithOffset(1, *chunk.ExitStatus).To(Equal(uint32(status)))
				}
			}
		}

		runInContainer := func(container warden.Container, script string) <-chan warden.ProcessStream {
			_, stream, err := container.Run(warden.ProcessSpec{Script: script})
			Expect(err).ToNot(HaveOccurred())

			return stream
		}

		It("makes that block of ip addresses inaccessible to the container", func() {
			runInContainer(blockedListener, "nc -l 12345")
			runInContainer(unblockedListener, "nc -l 12345")
			runInContainer(allowedListener, "nc -l 12345")

			senderStream := runInContainer(
				sender,
				fmt.Sprintf("echo hello | nc -w 1 %s 12345", blockedListenerIP),
			)
			expectStreamToExitWith(senderStream, 1)

			senderStream = runInContainer(
				sender,
				fmt.Sprintf("echo hello | nc -w 1 %s 12345", unblockedListenerIP),
			)
			expectStreamToExitWith(senderStream, 0)

			senderStream = runInContainer(
				sender,
				fmt.Sprintf("echo hello | nc -w 1 %s 12345", allowedListenerIP),
			)
			expectStreamToExitWith(senderStream, 0)
		})
	})
})
