/*
 * Copyright (c) 2020 Devtron Labs
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"github.com/caarlos0/env"
	pubsub "github.com/devtron-labs/common-lib/pubsub-lib"
	"github.com/devtron-labs/git-sensor/api"
	"github.com/devtron-labs/git-sensor/bean"
	"github.com/devtron-labs/git-sensor/internal/middleware"
	"github.com/devtron-labs/git-sensor/pkg/git"
	pb "github.com/devtron-labs/protos/git-sensor"
	"github.com/go-pg/pg"
	"github.com/gorilla/handlers"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

type App struct {
	MuxRouter          *api.MuxRouter
	Logger             *zap.SugaredLogger
	watcher            *git.GitWatcherImpl
	restServer         *http.Server
	grpcServer         *grpc.Server
	db                 *pg.DB
	pubSubClient       *pubsub.PubSubClientServiceImpl
	GrpcControllerImpl *api.GrpcControllerImpl
	StartupConfig      *bean.StartupConfig
}

func NewApp(MuxRouter *api.MuxRouter, Logger *zap.SugaredLogger, impl *git.GitWatcherImpl, db *pg.DB, pubSubClient *pubsub.PubSubClientServiceImpl, GrpcControllerImpl *api.GrpcControllerImpl) *App {
	return &App{
		MuxRouter:          MuxRouter,
		Logger:             Logger,
		watcher:            impl,
		db:                 db,
		pubSubClient:       pubSubClient,
		GrpcControllerImpl: GrpcControllerImpl,
	}
}

type PanicLogger struct {
	Logger *zap.SugaredLogger
}

func (impl *PanicLogger) Println(param ...interface{}) {
	impl.Logger.Errorw("PANIC", "err", param)
	middleware.PanicCounter.WithLabelValues().Inc()
}

func (app *App) Start() {

	// Parse configuration
	app.StartupConfig = &bean.StartupConfig{}
	err := env.Parse(app.StartupConfig)
	if err != nil {
		app.Logger.Errorw("failed to parse configuration")
		os.Exit(2)
	}

	app.Logger.Infow("server protocol configured "+app.StartupConfig.Protocol,
		"protocol", app.StartupConfig.Protocol)

	if app.StartupConfig.Protocol == "GRPC" {
		err = app.initGrpcServer(app.StartupConfig.Port)

	} else if app.StartupConfig.Protocol == "REST" {
		err = app.initRestServer(app.StartupConfig.Port)

	} else {
		app.Logger.Errorw("unknown protocol provided", "protocol", app.StartupConfig.Protocol)
		os.Exit(2)
	}

	// Stop if encountered error
	if err != nil {
		app.Logger.Errorw("error in startup", "err", err)
		os.Exit(2)
	}
}

func (app *App) initRestServer(port int) error {
	app.Logger.Infow("rest server starting", "port", app.StartupConfig.Port)
	app.MuxRouter.Init()
	//authEnforcer := casbin2.Create()

	h := handlers.RecoveryHandler(handlers.RecoveryLogger(&PanicLogger{Logger: app.Logger}))(app.MuxRouter.Router)

	app.restServer = &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: h}
	app.MuxRouter.Router.Use(middleware.PrometheusMiddleware)

	return app.restServer.ListenAndServe()
}

func (app *App) initGrpcServer(port int) error {
	app.Logger.Infow("gRPC server starting", "port", app.StartupConfig.Port)

	//listen on the port
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to start grpcServer %v", err)
		return err
	}

	// create a new gRPC grpcServer
	app.grpcServer = grpc.NewServer()

	// register GitSensor service
	pb.RegisterGitSensorServiceServer(app.grpcServer, app.GrpcControllerImpl)

	// start listening on address
	if err = app.grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to start: %v", err)
		return err
	}

	log.Printf("grpcServer started at %v", lis.Addr())
	return nil
}

// Stop stops the grpcServer and cleans resources. Called during shutdown
func (app *App) Stop() {
	app.Logger.Infow("orchestrator shutdown initiating")

	app.Logger.Infow("stopping cron")
	app.watcher.StopCron()

	if app.StartupConfig.Protocol == "GRPC" {
		app.Logger.Infow("gracefully stopping GitSensor")
		app.grpcServer.GracefulStop()

	} else {
		timeoutContext, _ := context.WithTimeout(context.Background(), 5*time.Second)
		app.Logger.Infow("closing router")
		err := app.restServer.Shutdown(timeoutContext)
		if err != nil {
			app.Logger.Errorw("error in mux router shutdown", "err", err)
		}
	}

	app.Logger.Infow("closing db connection")
	err := app.db.Close()
	if err != nil {
		app.Logger.Errorw("error in closing db connection", "err", err)
	}

	app.Logger.Infow("housekeeping done. exiting now")
}
