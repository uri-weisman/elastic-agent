// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package application

import (
	"context"
	"path/filepath"

	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/filters"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/info"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/paths"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/pipeline"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/pipeline/emitter"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/pipeline/emitter/modifiers"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/pipeline/router"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/pipeline/stream"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/application/upgrade"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/configuration"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/errors"
	"github.com/elastic/elastic-agent-poc/internal/pkg/agent/operation"
	"github.com/elastic/elastic-agent-poc/internal/pkg/capabilities"
	"github.com/elastic/elastic-agent-poc/internal/pkg/composable"
	"github.com/elastic/elastic-agent-poc/internal/pkg/config"
	"github.com/elastic/elastic-agent-poc/internal/pkg/core/logger"
	"github.com/elastic/elastic-agent-poc/internal/pkg/core/monitoring"
	"github.com/elastic/elastic-agent-poc/internal/pkg/core/server"
	"github.com/elastic/elastic-agent-poc/internal/pkg/core/status"
	"github.com/elastic/elastic-agent-poc/internal/pkg/dir"
	acker "github.com/elastic/elastic-agent-poc/internal/pkg/fleetapi/acker/noop"
	reporting "github.com/elastic/elastic-agent-poc/internal/pkg/reporter"
	logreporter "github.com/elastic/elastic-agent-poc/internal/pkg/reporter/log"
	"github.com/elastic/elastic-agent-poc/internal/pkg/sorted"
)

type discoverFunc func() ([]string, error)

// ErrNoConfiguration is returned when no configuration are found.
var ErrNoConfiguration = errors.New("no configuration found", errors.TypeConfig)

// Local represents a standalone agents, that will read his configuration directly from disk.
// Some part of the configuration can be reloaded.
type Local struct {
	bgContext   context.Context
	cancelCtxFn context.CancelFunc
	log         *logger.Logger
	router      pipeline.Router
	source      source
	agentInfo   *info.AgentInfo
	srv         *server.Server
}

type source interface {
	Start() error
	Stop() error
}

// newLocal return a agent managed by local configuration.
func newLocal(
	ctx context.Context,
	log *logger.Logger,
	pathConfigFile string,
	rawConfig *config.Config,
	reexec reexecManager,
	statusCtrl status.Controller,
	uc upgraderControl,
	agentInfo *info.AgentInfo,
) (*Local, error) {
	caps, err := capabilities.Load(paths.AgentCapabilitiesPath(), log, statusCtrl)
	if err != nil {
		return nil, err
	}

	cfg, err := configuration.NewFromConfig(rawConfig)
	if err != nil {
		return nil, err
	}

	if log == nil {
		log, err = logger.NewFromConfig("", cfg.Settings.LoggingConfig, true)
		if err != nil {
			return nil, err
		}
	}

	logR := logreporter.NewReporter(log)

	localApplication := &Local{
		log:       log,
		agentInfo: agentInfo,
	}

	localApplication.bgContext, localApplication.cancelCtxFn = context.WithCancel(ctx)
	localApplication.srv, err = server.NewFromConfig(log, cfg.Settings.GRPC, &operation.ApplicationStatusHandler{})
	if err != nil {
		return nil, errors.New(err, "initialize GRPC listener")
	}

	reporter := reporting.NewReporter(localApplication.bgContext, log, localApplication.agentInfo, logR)

	monitor, err := monitoring.NewMonitor(cfg.Settings)
	if err != nil {
		return nil, errors.New(err, "failed to initialize monitoring")
	}

	router, err := router.New(log, stream.Factory(localApplication.bgContext, agentInfo, cfg.Settings, localApplication.srv, reporter, monitor, statusCtrl))
	if err != nil {
		return nil, errors.New(err, "fail to initialize pipeline router")
	}
	localApplication.router = router

	composableCtrl, err := composable.New(log, rawConfig)
	if err != nil {
		return nil, errors.New(err, "failed to initialize composable controller")
	}

	discover := discoverer(pathConfigFile, cfg.Settings.Path, externalConfigsGlob())
	emit, err := emitter.New(
		localApplication.bgContext,
		log,
		agentInfo,
		composableCtrl,
		router,
		&pipeline.ConfigModifiers{
			Decorators: []pipeline.DecoratorFunc{modifiers.InjectMonitoring},
			Filters:    []pipeline.FilterFunc{filters.StreamChecker},
		},
		caps,
		monitor,
	)
	if err != nil {
		return nil, err
	}

	loader := config.NewLoader(log, externalConfigsGlob())

	var cfgSource source
	if !cfg.Settings.Reload.Enabled {
		log.Debug("Reloading of configuration is off")
		cfgSource = newOnce(log, discover, loader, emit)
	} else {
		log.Debugf("Reloading of configuration is on, frequency is set to %s", cfg.Settings.Reload.Period)
		cfgSource = newPeriodic(log, cfg.Settings.Reload.Period, discover, loader, emit)
	}

	localApplication.source = cfgSource

	// create a upgrader to use in local mode
	upgrader := upgrade.NewUpgrader(
		agentInfo,
		cfg.Settings.DownloadConfig,
		log,
		[]context.CancelFunc{localApplication.cancelCtxFn},
		reexec,
		acker.NewAcker(),
		reporter,
		caps)
	uc.SetUpgrader(upgrader)

	return localApplication, nil
}

func externalConfigsGlob() string {
	return filepath.Join(paths.Config(), configuration.ExternalInputsPattern)
}

// Routes returns a list of routes handled by agent.
func (l *Local) Routes() *sorted.Set {
	return l.router.Routes()
}

// Start starts a local agent.
func (l *Local) Start() error {
	l.log.Info("Agent is starting")
	defer l.log.Info("Agent is stopped")

	if err := l.srv.Start(); err != nil {
		return err
	}
	if err := l.source.Start(); err != nil {
		return err
	}

	return nil
}

// Stop stops a local agent.
func (l *Local) Stop() error {
	err := l.source.Stop()
	l.cancelCtxFn()
	l.router.Shutdown()
	l.srv.Stop()
	return err
}

// AgentInfo retrieves agent information.
func (l *Local) AgentInfo() *info.AgentInfo {
	return l.agentInfo
}

func discoverer(patterns ...string) discoverFunc {
	var p []string
	for _, newP := range patterns {
		if len(newP) == 0 {
			continue
		}

		p = append(p, newP)
	}

	if len(p) == 0 {
		return func() ([]string, error) {
			return []string{}, ErrNoConfiguration
		}
	}

	return func() ([]string, error) {
		return dir.DiscoverFiles(p...)
	}
}