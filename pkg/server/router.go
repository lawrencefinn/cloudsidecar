package server

import (
	awshandler "cloudsidecar/pkg/aws/handler"
	kinesishandler "cloudsidecar/pkg/aws/handler/kinesis"
	s3handler "cloudsidecar/pkg/aws/handler/s3"
	"cloudsidecar/pkg/aws/handler/s3/bucket"
	"cloudsidecar/pkg/aws/handler/s3/object"
	csSqs "cloudsidecar/pkg/aws/handler/sqs"
	conf "cloudsidecar/pkg/config"
	"cloudsidecar/pkg/enterprise"
	gcpHandler "cloudsidecar/pkg/gcp/handler"
	gcsHandler "cloudsidecar/pkg/gcp/handler/gcs"
	gcsBucket "cloudsidecar/pkg/gcp/handler/gcs/bucket"
	gcsObject "cloudsidecar/pkg/gcp/handler/gcs/object"
	"cloudsidecar/pkg/logging"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/defaults"
	"github.com/aws/aws-sdk-go-v2/aws/endpoints"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gorilla/mux"
	"github.com/spf13/viper"
	"golang.org/x/net/http2"
	"net/http"
	"plugin"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Lock to run listen function only once
var listenLock sync.Mutex

// Router with a lock
type RouteWrapper struct {
	router *RouterWithCounter
	mutex  sync.Mutex
}

// Mux router with counter to make sure we don't close anything in use
type RouterWithCounter struct {
	mux             *mux.Router
	currentRequests int32
}

// Interface implementation gets called on each request that keeps track of current request count
func (router *RouterWithCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&router.currentRequests, 1)
	router.mux.ServeHTTP(w, r)
	atomic.AddInt32(&router.currentRequests, -1)
}

// Shutdown a handler if there are no current requests
func (router *RouterWithCounter) ShutdownWhenReady(handler awshandler.HandlerInterface) {
	for {
		if atomic.LoadInt32(&router.currentRequests) <= 0 {
			handler.Shutdown()
			return
		}
		if duration, err := time.ParseDuration("1s"); err == nil {
			time.Sleep(duration)
		}
	}
}

// Switch router when config changes
func (wrapper *RouteWrapper) ChangeRouter(newRouter *RouterWithCounter, oldHandler awshandler.HandlerInterface) {
	wrapper.mutex.Lock()
	oldRouter := wrapper.router
	wrapper.router = newRouter
	wrapper.mutex.Unlock()
	go oldRouter.ShutdownWhenReady(oldHandler)
}

// Interface implementation gets called on each request.  Makes sure to use a lock in case router changes
func (wrapper *RouteWrapper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wrapper.mutex.Lock()
	router := wrapper.router
	wrapper.mutex.Unlock()
	router.ServeHTTP(w, r)
}

func createAWSConfigs(awsConfig *conf.AWSConfig) aws.Config {
	configs := defaults.Config()
	creds := aws.NewStaticCredentialsProvider(awsConfig.DestinationAWSConfig.AccessKeyId, awsConfig.DestinationAWSConfig.SecretAccessKey, "")
	configs.Credentials = creds
	configs.Region = endpoints.UsEast1RegionID
	return configs
}

// Create a handler from config
func CreateHandlerGCP(key string, gcpConfig *conf.AWSConfig, enterpriseSystem enterprise.Enterprise, serverWaitGroup *sync.WaitGroup) (handler awshandler.HandlerInterface, router *mux.Router, toListen bool) {
	var gcpHandler gcpHandler.HandlerInterface
	toListen = true
	r := mux.NewRouter()
	r.Use(loggingMiddleware)
	ctx := context.Background()
	if gcpConfig.ServiceType == "gcs" {
		handler := gcsHandler.NewHandler(viper.Sub(fmt.Sprint("gcp_configs.", key)))
		if gcpConfig.DestinationAWSConfig != nil {
			configs := createAWSConfigs(gcpConfig)
			svc := s3.New(configs)
			handler.S3Client = svc
		}
		if gcpConfig.DestinationGCPConfig != nil {
			// use GCS
			var gcpClient func() (s3handler.GCPClient, error)
			if gcpConfig.DestinationGCPConfig.KeyFileLocation != nil {
				credInput := *gcpConfig.DestinationGCPConfig.KeyFileLocation
				gcpClient = func() (s3handler.GCPClient, error) {
					return newGCPStorage(ctx, credInput)
				}
			} else if gcpConfig.DestinationGCPConfig.KeyFromUrl != nil && *gcpConfig.DestinationGCPConfig.KeyFromUrl {
				gcpClient = func() (s3handler.GCPClient, error) {
					return newGCPStorageNoCreds(ctx)
				}
			} else {
				credInput := *gcpConfig.DestinationGCPConfig.RawKey
				gcpClient = func() (s3handler.GCPClient, error) {
					return newGCPStorageRawKey(ctx, credInput)
				}
			}
			handler.GCPClient = gcpClient
			handler.Context = &ctx
		}
		gcpHandler = &handler
		bucketHandler := gcsBucket.New(&handler)
		objectHandler := gcsObject.New(&handler)
		// register http handlers for bucket requests and object requests
		bucketHandler.Register(r)
		objectHandler.Register(r)
	}
	r.PathPrefix("/").HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		logging.Log.Info("Catch all %s %s %s", request.URL, request.Method, request.Header)
		writer.WriteHeader(404)
	})
	return gcpHandler, r, toListen
}

// Create a handler from config
func CreateHandlerAWS(key string, awsConfig *conf.AWSConfig, enterpriseSystem enterprise.Enterprise, serverWaitGroup *sync.WaitGroup) (handler awshandler.HandlerInterface, router *mux.Router, toListen bool) {
	var awsHandler awshandler.HandlerInterface
	toListen = true
	r := mux.NewRouter()
	r.Use(loggingMiddleware)
	ctx := context.Background()
	if awsConfig.ServiceType == "s3" {
		// set up generic handler for s3
		handler := s3handler.NewHandler(viper.Sub(fmt.Sprint("aws_configs.", key)))
		if awsConfig.DestinationAWSConfig != nil {
			configs := createAWSConfigs(awsConfig)
			svc := s3.New(configs)
			handler.S3Client = svc
		}
		if awsConfig.DestinationGCPConfig != nil {
			// use GCS
			var gcpClient func() (s3handler.GCPClient, error)
			if awsConfig.DestinationGCPConfig.KeyFileLocation != nil {
				credInput := *awsConfig.DestinationGCPConfig.KeyFileLocation
				gcpClient = func() (s3handler.GCPClient, error) {
					return newGCPStorage(ctx, credInput)
				}
			} else if awsConfig.DestinationGCPConfig.KeyFromUrl != nil && *awsConfig.DestinationGCPConfig.KeyFromUrl {
				gcpClient = func() (s3handler.GCPClient, error) {
					return newGCPStorageNoCreds(ctx)
				}
			} else {
				credInput := *awsConfig.DestinationGCPConfig.RawKey
				gcpClient = func() (s3handler.GCPClient, error) {
					return newGCPStorageRawKey(ctx, credInput)
				}
			}
			handler.GCPClient = gcpClient
			handler.Context = &ctx
		}
		bucketHandler := bucket.New(&handler)
		objectHandler := object.New(&handler)
		awsHandler = &handler
		// register http handlers for bucket requests and object requests
		bucketHandler.Register(r)
		objectHandler.Register(r)
	} else if awsConfig.ServiceType == "kinesis" {
		handler := kinesishandler.NewHandler(viper.Sub(fmt.Sprint("aws_configs.", key)))
		if awsConfig.DestinationAWSConfig != nil {
			configs := createAWSConfigs(awsConfig)
			svc := kinesis.New(configs)
			handler.KinesisClient = svc
		}
		if awsConfig.DestinationGCPConfig != nil {
			// use pubsub
			gcpClient, err := newGCPPubSub(
				ctx,
				awsConfig.DestinationGCPConfig.Project,
				*awsConfig.DestinationGCPConfig.KeyFileLocation,
			)
			if err != nil {
				panic(fmt.Sprintln("Error setting up gcp client", err))
			}
			gcpKmsClient, err := newGCPKmsClient(ctx, *awsConfig.DestinationGCPConfig.KeyFileLocation)
			if err != nil {
				panic(fmt.Sprintln("Error setting up gcp client", err))
			}
			handler.GCPClient = gcpClient
			handler.GCPKMSClient = gcpKmsClient
			handler.Context = &ctx
		}
		awsHandler = &handler
		wrappedHandler := kinesishandler.New(&handler)
		wrappedHandler.Register(r)
	} else if awsConfig.ServiceType == "sqs" {
		handler := csSqs.NewHandler(viper.Sub(fmt.Sprint("aws_configs.", key)))
		if awsConfig.DestinationAWSConfig != nil {
			configs := createAWSConfigs(awsConfig)
			svc := sqs.New(configs)
			handler.SqsClient = svc
		}
		if awsConfig.DestinationGCPConfig != nil {
			// use pubsub
			gcpClient, err := newGCPPubSub(
				ctx,
				awsConfig.DestinationGCPConfig.Project,
				*awsConfig.DestinationGCPConfig.KeyFileLocation,
			)
			if err != nil {
				panic(fmt.Sprintln("Error setting up gcp client", err))
			}
			gcpKmsClient, err := newGCPKmsClient(ctx, *awsConfig.DestinationGCPConfig.KeyFileLocation)
			if err != nil {
				panic(fmt.Sprintln("Error setting up gcp client", err))
			}
			handler.GCPClient = gcpClient
			handler.GCPKMSClient = gcpKmsClient
			handler.Context = &ctx
		}
		awsHandler = &handler
		handler.Register(r)
	} else if awsConfig.ServiceType == "" {
		logging.Log.Error("No service type configured for port ", awsConfig.Port)
	} else if enterpriseSystem.RegisterHandler(key, *awsConfig, r, serverWaitGroup) {
		toListen = false
		// do nothing, enterprise got this
	} else {
		// look for plugins for handlers
		plug, err := plugin.Open(fmt.Sprint("plugin/handler/", awsConfig.ServiceType, ".so"))
		if err != nil {
			logging.Log.Error("Cannot load plugin ", awsConfig.ServiceType, " for port ", awsConfig.Port, err)
			toListen = false
		} else {
			sym, symErr := plug.Lookup("Register")
			if symErr != nil {
				logging.Log.Error("Cannot call register from plugin", awsConfig.ServiceType, " ", symErr)
				toListen = false
			} else {
				registerFunc := sym.(func(*mux.Router) awshandler.HandlerInterface)
				handler := registerFunc(r)
				handler.SetConfig(viper.Sub(fmt.Sprint("aws_configs.", key)))
				handler.SetContext(&ctx)
				awsHandler = handler.(awshandler.HandlerInterface)
			}
		}
	}
	r.PathPrefix("/").HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		logging.Log.Info("Catch all %s %s %s", request.URL, request.Method, request.Header)
		writer.WriteHeader(404)
	})
	return awsHandler, r, toListen
}

// Listen for all configured services.  Gets called when started or configs change
func Listen(config *conf.Config, serverWaitGroup *sync.WaitGroup, enterpriseSystem enterprise.Enterprise) {
	// Only run one at a time
	listenLock.Lock()
	defer listenLock.Unlock()
	localAwsHandlers := make(map[string]awshandler.HandlerInterface)
	localGcpHandlers := make(map[string]gcpHandler.HandlerInterface)
	middlewares := getMiddlewares(enterpriseSystem, config)
	for key, gcpConfig := range config.GcpConfigs {
		gcpHandler, r, toListen := CreateHandlerGCP(key, &gcpConfig, enterpriseSystem, serverWaitGroup)
		oldHandler := gcpHandlers[key]
		if oldHandler != nil {
			logging.Log.Infof("Handler %s already exists, replacing", key)
		}
		localGcpHandlers[key] = gcpHandler
		port := gcpConfig.Port
		// Add in configured middlewares
		for _, middlewareName := range gcpConfig.Middleware {
			if middleware, ok := middlewares[middlewareName]; ok {
				r.Use(middleware)
			} else {
				logging.Log.Error("Could not find middleware ", middlewareName)
			}
		}
		// listen for all handlers
		if toListen {
			routewrapper := &RouteWrapper{
				router: &RouterWithCounter{
					mux: r,
				},
			}
			if existingRouter, ok := routes[key]; ok {
				logging.Log.Debug("Route existed", key)
				existingRouter.ChangeRouter(&RouterWithCounter{
					mux: r,
				}, oldHandler)
				routewrapper = existingRouter
			}
			routes[key] = routewrapper
			h2s := &http2.Server{}
			srv := &http.Server{
				Handler: routewrapper,
				Addr:    fmt.Sprintf("127.0.0.1:%d", port),
			}
			h2sConfigErr := http2.ConfigureServer(srv, h2s)
			if h2sConfigErr != nil {
				logging.Log.Errorf("Could not set up http2 %v", h2sConfigErr)
				continue
			}
			logging.Log.Debug("Listening on %s", srv.Addr)
			if existingSrv, ok := gcpServers[key]; ok {
				if existingSrv.Addr != srv.Addr {
					logging.Log.Errorf("Cannot change a bind address on config %s from %s to %s", key, existingSrv.Addr, srv.Addr)
					continue
				}
			} else {
				serverWaitGroup.Add(1)
				gcpServers[key] = srv
				go func() {
					// listenErr := srv.ListenAndServeTLS("server.crt", "server.key")
					listenErr := srv.ListenAndServe()
					logging.Log.Error("", listenErr)
					if (*config).PanicOnBindError && strings.Contains(listenErr.Error(), "bind: address already in use") {
						panic("Could not bind, exiting")
					}
					gcpHandler.Shutdown()
					serverWaitGroup.Done()
				}()
			}
		}
	}
	// for each configured aws config, we want to set up an http listener
	for key, awsConfig := range config.AwsConfigs {
		awsHandler, r, toListen := CreateHandlerAWS(key, &awsConfig, enterpriseSystem, serverWaitGroup)
		oldHandler := awsHandlers[key]
		if oldHandler != nil {
			logging.Log.Infof("Handler %s already exists, replacing", key)
		}
		localAwsHandlers[key] = awsHandler
		port := awsConfig.Port
		// Add in configured middlewares
		for _, middlewareName := range awsConfig.Middleware {
			if middleware, ok := middlewares[middlewareName]; ok {
				r.Use(middleware)
			} else {
				logging.Log.Error("Could not find middleware ", middlewareName)
			}
		}
		// listen for all handlers
		if toListen {
			routewrapper := &RouteWrapper{
				router: &RouterWithCounter{
					mux: r,
				},
			}
			if existingRouter, ok := routes[key]; ok {
				logging.Log.Debug("Route existed", key)
				existingRouter.ChangeRouter(&RouterWithCounter{
					mux: r,
				}, oldHandler)
				routewrapper = existingRouter
			}
			routes[key] = routewrapper
			srv := &http.Server{
				Handler: routewrapper,
				Addr:    fmt.Sprintf("127.0.0.1:%d", port),
			}
			logging.Log.Debug("Listening on %s", srv.Addr)
			if existingSrv, ok := awsServers[key]; ok {
				if existingSrv.Addr != srv.Addr {
					logging.Log.Errorf("Cannot change a bind address on config %s from %s to %s", key, existingSrv.Addr, srv.Addr)
					continue
				}
			} else {
				serverWaitGroup.Add(1)
				awsServers[key] = srv
				go func() {
					listenErr := srv.ListenAndServe()
					logging.Log.Error("", listenErr)
					if (*config).PanicOnBindError && strings.Contains(listenErr.Error(), "bind: address already in use") {
						panic("Could not bind, exiting")
					}
					awsHandler.Shutdown()
					serverWaitGroup.Done()
				}()
			}
		}
	}
	for key, srv := range awsServers {
		if _, ok := config.AwsConfigs[key]; !ok {
			logging.Log.Infof("Removing server %s on %s", key, srv.Addr)
			srv.Close()
			delete(awsServers, key)
			delete(routes, key)
			delete(awsHandlers, key)
		}
	}
	for key, srv := range gcpServers {
		if _, ok := config.GcpConfigs[key]; !ok {
			logging.Log.Infof("Removing server %s on %s", key, srv.Addr)
			srv.Close()
			delete(awsServers, key)
			delete(routes, key)
			delete(awsHandlers, key)
		}
	}
	awsHandlers = localAwsHandlers
	gcpHandlers = localGcpHandlers
}
