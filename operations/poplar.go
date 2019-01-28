package operations

import (
	"context"
	"fmt"
	"net"

	"github.com/evergreen-ci/poplar"
	"github.com/evergreen-ci/poplar/rpc"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/recovery"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	envVarPoplarRecorderRPCPort  = "POPLAR_RPC_PORT"
	envVarPoplarRecorderRPCHost  = "POPLAR_RPC_HOST"
	defaultPoplarRecorderRPCPort = 2288
)

func Poplar() cli.Command {
	return cli.Command{
		Name:  "poplar",
		Usage: "a performance testing and metrics reporting toolkit",
		Flags: []cli.Flag{},
		Subcommands: []cli.Command{
			poplarReport(),
			poplarGRPC(),
		},
	}
}

func poplarGRPC() cli.Command {
	return cli.Command{
		Name:  "grpc",
		Usage: "run an RPC service for accumulating raw event payloads",
		Flags: []cli.Flag{
			cli.IntFlag{
				Name:   "port",
				EnvVar: envVarPoplarRecorderRPCPort,
				Value:  defaultPoplarRecorderRPCPort,
			},
			cli.StringFlag{
				Name:   "host",
				EnvVar: envVarJasperRPCHost,
				Value:  envVarPoplarRecorderRPCHost,
			},
		},
		Action: func(c *cli.Context) error {
			port := c.Int("port")
			host := c.String("host")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			addr := fmt.Sprintf("%s:%d", host, port)
			lis, err := net.Listen("tcp", addr)
			if err != nil {
				return errors.WithStack(err)
			}

			grip.Infof("starting poplar gRPC service at '%s'", addr)
			rpcSrv := grpc.NewServer()

			registry := poplar.NewRegistry()
			if err = rpc.AttachService(registry, rpcSrv); err != nil {
				return errors.Wrap(err, "problem building service")
			}

			go signalListener(ctx, cancel)
			go func() { grip.Warning(rpcSrv.Serve(lis)) }()

			wait := make(chan struct{})

			go func() {
				defer close(wait)
				defer recovery.LogStackTraceAndContinue("waiting for rpc service")
				<-ctx.Done()
				rpcSrv.Stop()
				grip.Info("poplar rpc service terminated")
			}()

			<-wait
			return nil
		},
	}
}

func poplarReport() cli.Command {
	const (
		serviceFlagName  = "service"
		pathFlagName     = "path"
		insecureFlagName = "insecure"
		certFileFlagName = "certfile"
	)

	return cli.Command{
		Name:  "send",
		Usage: "send a metrics report",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  serviceFlagName,
				Usage: "specify the address of the metrics service",
			},
			cli.BoolFlag{
				Name:  insecureFlagName,
				Usage: "disables certificate validation requirements",
			},
			cli.StringFlag{
				Name:  certFileFlagName,
				Usage: "specify the client certificate to connect over TLS",
			},
			cli.StringFlag{
				Name:  pathFlagName,
				Usage: "specify the path of the input file, may be the first positional argument",
			},
		},
		Before: mergeBeforeFuncs(
			requireStringFlag(serviceFlagName),
			requireFileOrPositional(pathFlagName),
		),
		Action: func(c *cli.Context) error {
			addr := c.String(serviceFlagName)
			fileName := c.String(pathFlagName)
			isInsecure := c.Bool(insecureFlagName)
			certFile := c.String(certFileFlagName)

			report, err := poplar.LoadReport(fileName)
			if err != nil {
				return errors.WithStack(err)
			}

			rpcOpts := []grpc.DialOption{}
			if isInsecure {
				rpcOpts = append(rpcOpts, grpc.WithInsecure())
			} else {
				creds, _ := credentials.NewClientTLSFromFile(certFile, "")
				rpcOpts = append(rpcOpts, grpc.WithTransportCredentials(creds))
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			conn, err := grpc.DialContext(ctx, addr, rpcOpts...)
			if err != nil {
				return errors.WithStack(err)
			}

			if err := rpc.UploadReport(ctx, report, conn); err != nil {
				return errors.WithStack(err)
			}

			return nil
		},
	}
}
