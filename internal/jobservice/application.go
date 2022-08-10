package jobservice

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/G-Research/armada/internal/common/auth/authorization"
	grpcCommon "github.com/G-Research/armada/internal/common/grpc"
	"github.com/G-Research/armada/internal/jobservice/configuration"
	"github.com/G-Research/armada/internal/jobservice/repository"

	"github.com/G-Research/armada/internal/jobservice/server"

	js "github.com/G-Research/armada/pkg/api/jobservice"
)

type App struct {
	// Configuration for jobService
	Config *configuration.JobServiceConfiguration
}

func New(config *configuration.JobServiceConfiguration) *App {
	return &App{
		Config: config,
	}
}

func (a *App) StartUp(ctx context.Context) error {
	config := a.Config

	// Setup an errgroup that cancels on any job failing or there being no active jobs.
	g, _ := errgroup.WithContext(ctx)

	grpcServer := grpcCommon.CreateGrpcServer(config.Grpc.KeepaliveParams, config.Grpc.KeepaliveEnforcementPolicy, []authorization.AuthService{&authorization.AnonymousAuthService{}})

	subscribedJobSets := make(map[string]*repository.SubscribeTable)
	jobStatusMap := repository.NewJobSetSubscriptions(subscribedJobSets)

	db, err := sql.Open("sqlite", config.DatabasePath)
	if err != nil {
		log.Fatalf("Error Opening Sqlite DB from %s %v", config.DatabasePath, err)
	}
	defer db.Close()
	sqlJobRepo := repository.NewSQLJobService(jobStatusMap, config, db)
	jobService := server.NewJobService(config, *sqlJobRepo)
	js.RegisterJobServiceServer(grpcServer, jobService)
	sqlJobRepo.CreateTable()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", config.GrpcPort))
	if err != nil {
		return err
	}

	g.Go(func() error {
		ticker := time.NewTicker(time.Duration(config.SubscribeJobSetTime) * time.Second)
		for range ticker.C {
			for _, value := range sqlJobRepo.GetSubscribedJobSets() {
				log.Infof("Subscribed job sets : %s", value)
				if sqlJobRepo.CheckToUnSubscribe(value.Queue, value.JobSet, config.SubscribeJobSetTime) {
					sqlJobRepo.CleanupJobSetAndJobs(value.Queue, value.JobSet)
				}
			}
		}
		return nil
	})
	g.Go(func() error {
		defer log.Infof("Stopping server.")

		log.Info("JobService service listening on ", config.GrpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
		return nil
	})

	g.Wait()

	return nil
}