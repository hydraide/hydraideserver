// Package server
package server

import (
	"context"
	"errors"
	"fmt"
	hydrapb "github.com/hydraide/hydraide/generated/hydraidepbgo"
	"github.com/hydraide/hydraidecore/filesystem"
	"github.com/hydraide/hydraidecore/settings"
	"github.com/hydraide/hydraidecore/zeus"
	"github.com/hydraide/hydraideserver/gateway"
	"github.com/hydraide/hydraideserver/observer"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"
)

const (
	maxDepth        = 1
	foldersPerLevel = 1000
)

type Configuration struct {
	CertificateCrtFile string // Server CRT file path
	CertificateKeyFile string // Server Key file path
	// Hydra settings
	HydraServerPort       int   // the port where the hydra server listens
	HydraMaxMessageSize   int   // the maximum message size in bytes
	DefaultCloseAfterIdle int64 // the default close after idle time in seconds
	DefaultWriteInterval  int64 // the default write interval time in seconds
	DefaultFileSize       int64 // the default file size in bytes
	SystemResourceLogging bool  // if true, the system resource usage is logged
}

type Server interface {
	// Start starts the microservice
	Start() error
	// Stop stops the microservice gracefully
	Stop()
	// IsHydraRunning returns true if the hydra server is running
	IsHydraRunning() bool
}

type server struct {
	configuration      *Configuration
	observerCancelFunc context.CancelFunc
	mu                 sync.RWMutex
	serverRunning      bool
	grpcServer         *grpc.Server
	zeusInterface      zeus.Zeus
	observerInterface  observer.Observer
}

func New(configuration *Configuration) Server {
	return &server{
		configuration: configuration,
	}
}

func (s *server) IsHydraRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.serverRunning
}

func (s *server) Start() error {

	log.Info("starting the hydra server...")

	// check if the server is already running
	s.mu.Lock()
	if s.serverRunning {
		s.mu.Unlock()
		return errors.New("hydra server is already running")
	}
	s.serverRunning = true
	s.mu.Unlock()

	settingsInterface := settings.New(maxDepth, foldersPerLevel)
	s.zeusInterface = zeus.New(settingsInterface, filesystem.New())
	s.zeusInterface.StartHydra()

	var ctx context.Context
	ctx, s.observerCancelFunc = context.WithCancel(context.Background())
	s.observerInterface = observer.New(ctx, s.configuration.SystemResourceLogging)

	grpcServer := gateway.Gateway{
		ObserverInterface:     s.observerInterface,
		SettingsInterface:     settingsInterface,
		ZeusInterface:         s.zeusInterface,
		DefaultCloseAfterIdle: s.configuration.DefaultCloseAfterIdle,
		DefaultWriteInterval:  s.configuration.DefaultWriteInterval,
		DefaultFileSize:       s.configuration.DefaultFileSize,
	}

	unaryInterceptor := func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {

		// Get the client's IP address
		clientIP := "unknown"
		if p, ok := peer.FromContext(ctx); ok {
			if addr, ok := p.Addr.(*net.TCPAddr); ok {
				clientIP = addr.IP.String()
			}
		}

		resp, err := handler(ctx, req)
		if err != nil {
			// GRPC Server error loggolása
			if os.Getenv("GRPC_SERVER_ERROR_LOGGING") == "true" {
				if grpcErr, ok := status.FromError(err); ok {
					logFields := log.Fields{
						"method":   info.FullMethod,
						"clientIP": clientIP,
						"error":    grpcErr.Message(),
					}
					switch grpcErr.Code() {
					case codes.PermissionDenied:
						log.WithFields(logFields).Error("client request rejected: permission denied")
					case codes.Unauthenticated:
						log.WithFields(logFields).Error("client request rejected: unauthenticated")
					case codes.InvalidArgument:
						log.WithFields(logFields).Trace("client request rejected: invalid argument")
					case codes.ResourceExhausted:
						log.WithFields(logFields).Error("client request rejected: resource exhausted")
					case codes.FailedPrecondition:
						log.WithFields(logFields).Trace("client request rejected: failed precondition")
					case codes.Aborted:
						log.WithFields(logFields).Trace("client request rejected: aborted")
					case codes.OutOfRange:
						log.WithFields(logFields).Trace("client request rejected: out of range")
					case codes.Unavailable:
						log.WithFields(logFields).Error("client request rejected: unavailable")
					case codes.DataLoss:
						log.WithFields(logFields).Error("client request rejected: data loss")
					case codes.Unknown:
						log.WithFields(logFields).Trace("client request rejected: unknown error")
					case codes.Internal:
						log.WithFields(logFields).Error("client request rejected: internal server error")
					case codes.Unimplemented:
						log.WithFields(logFields).Warn("client request rejected: unimplemented")
					case codes.DeadlineExceeded:
						log.WithFields(logFields).Trace("client request rejected: deadline exceeded")
					case codes.Canceled:
						log.WithFields(logFields).Trace("client request rejected: canceled by client")
					default:
						log.WithFields(logFields).Error("client request rejected: unknown grpc error code")
					}
				} else {
					log.WithFields(log.Fields{
						"method":   info.FullMethod,
						"clientIP": clientIP,
						"error":    err.Error(),
					}).Warn("client request rejected: non-gRPC error")
				}
			}
		}
		return resp, err
	}

	// start the main server and waiting for incoming requests
	go func() {

		defer func() {
			if r := recover(); r != nil {
				// Lekérjük a stack trace-t
				stackTrace := debug.Stack()
				// Logoljuk a pánikot és a stack trace-t
				log.WithFields(log.Fields{
					"error": r,
					"stack": string(stackTrace),
				}).Error("caught panic in hydra gRPC server")
			}
		}()

		// start the gRPC server
		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.configuration.HydraServerPort))
		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Panic("can not create listener for the hydra server")
		}

		// load cert and key files for the server
		creds, err := credentials.NewServerTLSFromFile(s.configuration.CertificateCrtFile, s.configuration.CertificateKeyFile)
		if err != nil {
			log.Fatalf("failed to load TLS credentials: %v", err)
		}

		kaParams := keepalive.ServerParameters{
			// Ha egy kapcsolat inaktív 5 percig, a szerver lezárhatja azt.
			// Itt pl. 4 percnél küldünk keepalive üzenetet, hogy mindig legyen aktív forgalom.
			Time: 4 * time.Minute,
			// Ha a keepalive pingre 20 másodperc alatt nem érkezik válasz, lezárhatjuk a kapcsolatot.
			Timeout: 20 * time.Second,
			// Maximum inaktivitási idő, mielőtt a kapcsolatot lezárnánk.
			MaxConnectionIdle: 5 * time.Minute,
		}

		s.grpcServer = grpc.NewServer(
			grpc.Creds(creds),
			grpc.MaxSendMsgSize(s.configuration.HydraMaxMessageSize),
			grpc.MaxRecvMsgSize(s.configuration.HydraMaxMessageSize),
			grpc.UnaryInterceptor(unaryInterceptor), // add the interceptor
			grpc.KeepaliveParams(kaParams),          // Itt adjuk hozzá a keepalive paramétereket
		)

		// registering the server
		hydrapb.RegisterHydraideServiceServer(s.grpcServer, &grpcServer)

		log.Infof("hydra server is listening on port: %d", s.configuration.HydraServerPort)

		// create the server and start listening for requests
		if err = s.grpcServer.Serve(lis); err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("can not serve the server")
		}

	}()

	return nil

}

// Stop stops the microservice gracefully
func (s *server) Stop() {

	log.Info("server is stopping...")

	// check if the server is already stopped
	s.mu.Lock()
	if !s.serverRunning {
		s.mu.Unlock()
		log.Info("server is already stopped")
		return
	}
	s.serverRunning = false
	s.mu.Unlock()

	if s.grpcServer != nil {
		// stops the gRPC server gracefully because we don't want to get new requests from the crawler
		s.grpcServer.GracefulStop()
		log.Info("grpcServer has been stopped gracefully")
	}

	// waiting for all processes to finish. This is a blocker function until all processes are finished
	if s.observerInterface != nil {
		log.Info("waiting for all processes to finish in the background")
		s.observerInterface.WaitingForAllProcessesFinished()
		log.Info("all processes are finished in the background")
	}

	if s.zeusInterface != nil {
		// stop the Hydra gracefully. This is a blocker function until all swamps are stopped gracefully
		s.zeusInterface.StopHydra()
		log.Info("hydra has been stopped gracefully")
	}

	// stop the observer's monitoring process
	s.observerCancelFunc()

}
