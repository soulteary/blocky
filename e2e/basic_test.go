package e2e_test

import (
	"context"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"os"
)

var _ = Describe("Basic", func() {
	Describe("Testcontainer", func() {
		It("just test", func() {

			path, err := os.Getwd()
			Expect(err).Should(Succeed())

			ctx := context.Background()
			req := testcontainers.ContainerRequest{
				FromDockerfile: testcontainers.FromDockerfile{
					Context:       "../.",
					PrintBuildLog: true,
				},
				ExposedPorts: []string{"53/tcp", "53/udp"},
				BindMounts:   map[string]string{"/app/config.yml": path + "/basic.yml"},
				WaitingFor:   wait.ForListeningPort("53"),
			}

			container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				ContainerRequest: req,
				Started:          true,
			})

			Expect(err).Should(Succeed())
			defer container.Terminate(ctx)
		})
	})
})
