package cmd

import (
	"log"
	"time"

	"github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/longhorn/longhorn-engine/pkg/backend/dynamic"
	"github.com/longhorn/longhorn-engine/pkg/backend/file"
	"github.com/longhorn/longhorn-engine/pkg/backend/remote"
	"github.com/longhorn/longhorn-engine/pkg/controller"
	"github.com/longhorn/longhorn-engine/pkg/controller/client"
	controllerrpc "github.com/longhorn/longhorn-engine/pkg/controller/rpc"
	"github.com/longhorn/longhorn-engine/pkg/types"
	"github.com/longhorn/longhorn-engine/pkg/util"
)

func ControllerCmd() cli.Command {
	return cli.Command{
		Name: "controller",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "listen",
				Value: "localhost:9501",
			},
			cli.StringFlag{
				Name:  "size",
				Usage: "Volume nominal size in bytes or human readable 42kb, 42mb, 42gb",
			},
			cli.StringFlag{
				Name:  "current-size",
				Usage: "Volume current size in bytes or human readable 42kb, 42mb, 42gb",
			},
			cli.StringFlag{
				Name:  "frontend",
				Value: "",
			},
			cli.StringSliceFlag{
				Name:  "enable-backend",
				Value: (*cli.StringSlice)(&[]string{"tcp"}),
			},
			cli.StringSliceFlag{
				Name: "replica",
			},
			cli.BoolFlag{
				Name: "upgrade",
			},
			cli.BoolFlag{
				Name:   "disableRevCounter",
				Hidden: false,
				Usage:  "To disable revision counter checking",
			},
			cli.BoolFlag{
				Name:   "salvageRequested",
				Hidden: false,
				Usage:  "Start engine controller in a special mode only to get best replica candidate for salvage",
			},
			cli.Int64Flag{
				Name:   "engine-replica-timeout",
				Hidden: false,
				Usage:  "In seconds. Timeout between engine and replica(s)",
			},
			cli.StringFlag{
				Name:  "data-server-protocol",
				Value: "tcp",
				Usage: "Specify the data-server protocol. Available options are \"tcp\" and \"unix\"",
			},
		},
		Action: func(c *cli.Context) {
			if err := startController(c); err != nil {
				logrus.WithError(err).Fatalf("Error running controller command")
			}
		},
	}
}

func startController(c *cli.Context) error {
	if c.NArg() == 0 {
		return errors.New("volume name is required")
	}
	name := c.Args()[0]

	if !util.ValidVolumeName(name) {
		return errors.New("invalid target name")
	}

	listen := c.String("listen")
	backends := c.StringSlice("enable-backend")
	replicas := c.StringSlice("replica")
	frontendName := c.String("frontend")
	isUpgrade := c.Bool("upgrade")
	disableRevCounter := c.Bool("disableRevCounter")
	salvageRequested := c.Bool("salvageRequested")

	size := c.String("size")
	if size == "" {
		return errors.New("size is required")
	}
	volumeSize, err := units.RAMInBytes(size)
	if err != nil {
		return err
	}

	size = c.String("current-size")
	if size == "" {
		return errors.New("current-size is required")
	}
	volumeCurrentSize, err := units.RAMInBytes(size)
	if err != nil {
		return err
	}

	timeout := c.Int64("engine-replica-timeout")
	engineReplicaTimeout := time.Duration(timeout) * time.Second
	engineReplicaTimeout = controller.DetermineEngineReplicaTimeout(engineReplicaTimeout)
	iscsiTargetRequestTimeout := controller.DetermineIscsiTargetRequestTimeout(engineReplicaTimeout)

	dataServerProtocol := c.String("data-server-protocol")

	factories := map[string]types.BackendFactory{}
	for _, backend := range backends {
		switch backend {
		case "file":
			factories[backend] = file.New()
		case "tcp":
			factories[backend] = remote.New()
		default:
			logrus.Fatalf("Unsupported backend: %s", backend)
		}
	}

	var frontend types.Frontend
	if frontendName != "" {
		f, err := controller.NewFrontend(frontendName, iscsiTargetRequestTimeout)
		if err != nil {
			return errors.Wrapf(err, "failed to find frontend: %s", frontendName)
		}
		frontend = f
	}

	logrus.Infof("Creating controller %v with iSCSI target request timeout %v and engine to replica(s) timeout %v",
		name, iscsiTargetRequestTimeout, engineReplicaTimeout)
	control := controller.NewController(name, dynamic.New(factories), frontend, isUpgrade, disableRevCounter, salvageRequested,
		iscsiTargetRequestTimeout, engineReplicaTimeout, types.DataServerProtocol(dataServerProtocol))

	// need to wait for Shutdown() completion
	control.ShutdownWG.Add(1)
	addShutdown(func() (err error) {
		defer control.ShutdownWG.Done()
		logrus.Debugf("Starting to execute shutdown function for the engine controller of volume %v", name)
		return control.Shutdown()
	})

	if len(replicas) > 0 {
		logrus.Infof("Starting with replicas %q", replicas)
		if err := control.Start(volumeSize, volumeCurrentSize, replicas...); err != nil {
			log.Fatal(err)
		}
	}

	control.GRPCAddress = util.GetGRPCAddress(listen)
	control.GRPCServer = controllerrpc.GetControllerGRPCServer(control)

	control.StartGRPCServer()
	return control.WaitForShutdown()
}

func getControllerClient(c *cli.Context) (*client.ControllerClient, error) {
	url := c.GlobalString("url")
	return client.NewControllerClient(url)
}
