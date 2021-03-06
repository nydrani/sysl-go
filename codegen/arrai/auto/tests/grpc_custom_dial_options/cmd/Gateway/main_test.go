package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	ebpb "grpc_custom_dial_options/gen/pb/encoder_backend"
	pb "grpc_custom_dial_options/gen/pb/gateway"

	"github.com/anz-bank/pkg/log"
	"github.com/anz-bank/sysl-go/core"
	"github.com/sethvargo/go-retry"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

const applicationConfig = `---
genCode:
  upstream:
    grpc:
      hostName: "localhost"
      port: 9021 # FIXME no guarantee this port is free
  downstream:
    contextTimeout: "30s"
    encoder_backend:
      serviceAddress: localhost:9022
`

func doGatewayRequestResponse(ctx context.Context, content string) (string, error) {
	conn, err := grpc.Dial("localhost:9021", grpc.WithInsecure())
	if err != nil {
		fmt.Printf("test client failed to connect to gateway: %s\n", err.Error())
		return "", err
	}
	defer conn.Close()

	client := pb.NewGatewayClient(conn)
	response, err := client.Encode(ctx, &pb.EncodeRequest{Content: content, EncoderId: "rot13"})
	if err != nil {
		fmt.Printf("test client got error after making Encoding request to gateway: %s\n", err.Error())
		return "", err
	}
	return response.Content, nil
}

type dummyEncoderBackend struct{}

func (s dummyEncoderBackend) Rot13(ctx context.Context, req *ebpb.EncodingRequest) (*ebpb.EncodingResponse, error) {
	var md metadata.MD
	md, ok := metadata.FromIncomingContext(ctx)

	var tau int
	var err error
	if !ok {
		tau = 13
	} else {
		values := md.Get("rot-parameter-override")
		complaint := fmt.Errorf("rot-parameter-override metadata had unexpected value: %v, expected an integer", values)
		if len(values) != 1 {
			return nil, complaint
		}
		tau, err = strconv.Atoi(values[0])
		if err != nil {
			return nil, complaint
		}
	}

	// valuable business logic as used in our dummy implementation of EncoderBackend service
	toRotTau := make(map[rune]rune)
	az := []rune{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z'}
	for i, r := range az {
		j := (i + tau) % len(az)
		if j < 0 {
			j += len(az)
		}
		toRotTau[r] = az[j]
	}
	rot13 := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			s, ok := toRotTau[unicode.ToLower(r)]
			if ok {
				b.WriteRune(s)
			} else {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return &ebpb.EncodingResponse{Content: rot13(req.Content)}, nil
}

func grpcLogger(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	resp, err := handler(ctx, req)
	if err == nil {
		fmt.Printf("dummy encoder backend: Call %q OK", info.FullMethod)
	} else {
		fmt.Printf("dummy encoder backend: Call %q FAIL: %s", info.FullMethod, err)
	}
	return resp, err
}

func startDummyEncoderBackendServer(addr string) (stopServer func() error) {
	// Starts a hand-written implementation of the EncoderBackend service running on given TCP Address.
	// Returns a function that can be used to stop the server.

	server := grpc.NewServer(grpc.UnaryInterceptor(grpcLogger))
	ebpb.RegisterEncoderBackendServer(server, dummyEncoderBackend{})

	c := make(chan error, 1)

	go func() {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			c <- err
			return
		}
		fmt.Printf("dummy encoder backend server will listen on %s...\n", addr)
		c <- server.Serve(listener)
	}()

	stopServer = func() error {
		// If the server stopped with some error before the caller
		// tried to stop it, return that error instead.
		select {
		case err := <-c:
			return err
		default:
		}
		server.Stop()
		return nil
	}
	return stopServer
}

func TestSimpleGRPCWithDownstreamAppSmokeTest(t *testing.T) {
	// Initialise context with pkg logger
	logger := log.NewStandardLogger()
	ctx := log.WithLogger(logger).Onto(context.Background())

	// Add in a fake filesystem to pass in config
	memFs := afero.NewMemMapFs()
	err := afero.Afero{Fs: memFs}.WriteFile("config.yaml", []byte(applicationConfig), 0777)
	require.NoError(t, err)
	ctx = core.ConfigFileSystemOnto(ctx, memFs)

	// FIXME patch core.Serve to allow it to optionally load app config path from ctx
	args := os.Args
	defer func() { os.Args = args }()
	os.Args = []string{"./gateway.out", "config.yaml"}

	// Start the dummy encoder backend service running
	stopEncoderBackendServer := startDummyEncoderBackendServer("localhost:9022")
	defer func() {
		err := stopEncoderBackendServer()
		require.NoError(t, err)
	}()

	// Start gateway application running as server
	go func() {
		err := application(ctx)
		if err != nil {
			panic(err)
		}
	}()

	// Wait for application to come up
	backoff, err := retry.NewFibonacci(20 * time.Millisecond)
	require.NoError(t, err)
	backoff = retry.WithMaxDuration(5*time.Second, backoff)
	err = retry.Do(ctx, backoff, func(ctx context.Context) error {
		_, err := doGatewayRequestResponse(ctx, "testing; one two, one two; is this thing on?")
		if err != nil {
			return retry.RetryableError(err)
		}
		return nil
	})
	require.NoError(t, err)

	// Test if the endpoint of our gateway application server works

	// note: expected = rot17("hello world") . the parameter 17 is chosen by a custom grpc.DialOption injected
	// by main.go using the hook AdditionalGrpcDialOptions.
	expected := "yvccf nficu"

	actual, err := doGatewayRequestResponse(ctx, "hello world")
	require.Nil(t, err)
	require.Equal(t, expected, actual)

	// FIXME how do we stop the application server?
}
