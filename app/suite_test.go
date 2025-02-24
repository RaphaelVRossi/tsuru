// Copyright 2012 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/app/version"
	"github.com/tsuru/tsuru/applog"
	"github.com/tsuru/tsuru/auth"
	"github.com/tsuru/tsuru/auth/native"
	"github.com/tsuru/tsuru/builder"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/dbtest"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/pool"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"github.com/tsuru/tsuru/queue"
	"github.com/tsuru/tsuru/router/rebuild"
	"github.com/tsuru/tsuru/router/routertest"
	"github.com/tsuru/tsuru/servicemanager"
	servicemock "github.com/tsuru/tsuru/servicemanager/mock"
	_ "github.com/tsuru/tsuru/storage/mongodb"
	appTypes "github.com/tsuru/tsuru/types/app"
	authTypes "github.com/tsuru/tsuru/types/auth"
	"github.com/tsuru/tsuru/types/quota"
	"github.com/tsuru/tsuru/volume"
	"golang.org/x/crypto/bcrypt"
	check "gopkg.in/check.v1"
)

func Test(t *testing.T) { check.TestingT(t) }

type S struct {
	conn        *db.Storage
	team        authTypes.Team
	user        *auth.User
	plan        appTypes.Plan
	defaultPlan appTypes.Plan
	provisioner *provisiontest.FakeProvisioner
	builder     *builder.MockBuilder
	Pool        string
	zeroLock    map[string]interface{}
	mockService servicemock.MockService
}

var _ = check.Suite(&S{})

type greaterChecker struct{}

func (c *greaterChecker) Info() *check.CheckerInfo {
	return &check.CheckerInfo{Name: "Greater", Params: []string{"expected", "obtained"}}
}

func (c *greaterChecker) Check(params []interface{}, names []string) (bool, string) {
	if len(params) != 2 {
		return false, "you should pass two values to compare"
	}
	n1, ok := params[0].(int)
	if !ok {
		return false, "first parameter should be int"
	}
	n2, ok := params[1].(int)
	if !ok {
		return false, "second parameter should be int"
	}
	if n1 > n2 {
		return true, ""
	}
	err := fmt.Sprintf("%d is not greater than %d", params[0], params[1])
	return false, err
}

var Greater check.Checker = &greaterChecker{}

func (s *S) createUserAndTeam(c *check.C) {
	s.user = &auth.User{
		Email: "whydidifall@thewho.com",
		Quota: quota.UnlimitedQuota,
	}
	err := s.user.Create()
	c.Assert(err, check.IsNil)
	s.team = authTypes.Team{
		Name:  "tsuruteam",
		Quota: quota.UnlimitedQuota,
	}
}

var nativeScheme = auth.Scheme(native.NativeScheme{})

func (s *S) SetUpSuite(c *check.C) {
	TestLogWriterWaitOnClose = true
	err := config.ReadConfigFile("testdata/config.yaml")
	c.Assert(err, check.IsNil)
	config.Set("log:disable-syslog", true)
	config.Set("queue:mongo-url", "127.0.0.1:27017?maxPoolSize=100")
	config.Set("queue:mongo-database", "queue_app_pkg_tests")
	config.Set("queue:mongo-polling-interval", 0.01)
	config.Set("docker:registry", "registry.somewhere")
	config.Set("routers:fake-tls:type", "fake-tls")
	config.Set("routers:fake-v2:type", "fake-v2")
	config.Set("auth:hash-cost", bcrypt.MinCost)
	s.conn, err = db.Conn()
	c.Assert(err, check.IsNil)
	s.provisioner = provisiontest.ProvisionerInstance
	provision.DefaultProvisioner = "fake"
	AuthScheme = nativeScheme
	data, err := json.Marshal(appTypes.AppLock{})
	c.Assert(err, check.IsNil)
	err = json.Unmarshal(data, &s.zeroLock)
	c.Assert(err, check.IsNil)
}

func (s *S) TearDownSuite(c *check.C) {
	defer s.conn.Close()
	dbtest.ClearAllCollections(s.conn.Apps().Database)
}

func (s *S) SetUpTest(c *check.C) {
	// Reset fake routers twice, first time will remove registered failures and
	// allow pending enqueued tasks to run, second time (after queue is reset)
	// will remove any routes added by executed queue tasks.
	routertest.FakeRouter.Reset()
	routertest.HCRouter.Reset()
	routertest.TLSRouter.Reset()
	routertest.OptsRouter.Reset()
	queue.ResetQueue()
	rebuild.Shutdown(context.Background())
	routertest.FakeRouter.Reset()
	routertest.HCRouter.Reset()
	routertest.TLSRouter.Reset()
	routertest.OptsRouter.Reset()
	pool.ResetCache()
	err := rebuild.Initialize(func(appName string) (rebuild.RebuildApp, error) {
		a, err := GetByName(context.TODO(), appName)
		if err == appTypes.ErrAppNotFound {
			return nil, nil
		}
		return a, err
	})
	c.Assert(err, check.IsNil)
	config.Set("docker:router", "fake")
	s.provisioner.Reset()
	dbtest.ClearAllCollections(s.conn.Apps().Database)
	s.createUserAndTeam(c)
	s.defaultPlan = appTypes.Plan{
		Name:     "default-plan",
		Memory:   1024,
		Swap:     1024,
		CpuShare: 100,
		Default:  true,
	}
	s.plan = appTypes.Plan{}
	s.Pool = "pool1"
	opts := pool.AddPoolOptions{Name: s.Pool, Default: true}
	err = pool.AddPool(context.TODO(), opts)
	c.Assert(err, check.IsNil)
	s.builder = &builder.MockBuilder{}
	builder.Register("fake", s.builder)
	builder.DefaultBuilder = "fake"
	setupMocks(s)
	servicemanager.App, err = AppService()
	c.Assert(err, check.IsNil)
	servicemanager.AppLog, err = applog.AppLogService()
	c.Assert(err, check.IsNil)
	servicemanager.AppVersion, err = version.AppVersionService()
	c.Assert(err, check.IsNil)
	servicemanager.Volume, err = volume.VolumeService()
	c.Assert(err, check.IsNil)
}

func (s *S) TearDownTest(c *check.C) {
	GetAppRouterUpdater().Shutdown(context.Background())
}

func setupMocks(s *S) {
	servicemock.SetMockService(&s.mockService)

	s.mockService.Team.OnList = func() ([]authTypes.Team, error) {
		return []authTypes.Team{{Name: s.team.Name}}, nil
	}
	s.mockService.Team.OnFindByName = func(name string) (*authTypes.Team, error) {
		if name == s.team.Name {
			return &authTypes.Team{Name: s.team.Name}, nil
		}
		return nil, authTypes.ErrTeamNotFound
	}
	s.mockService.Team.OnFindByNames = func(names []string) ([]authTypes.Team, error) {
		if len(names) == 1 && names[0] == s.team.Name {
			return []authTypes.Team{{Name: s.team.Name}}, nil
		}
		return []authTypes.Team{}, nil
	}

	s.mockService.Plan.OnList = func() ([]appTypes.Plan, error) {
		if s.plan.Name != "" {
			return []appTypes.Plan{s.defaultPlan, s.plan}, nil
		}
		return []appTypes.Plan{s.defaultPlan}, nil
	}
	s.mockService.Plan.OnDefaultPlan = func() (*appTypes.Plan, error) {
		return &s.defaultPlan, nil
	}
	s.mockService.Plan.OnFindByName = func(name string) (*appTypes.Plan, error) {
		if name == s.defaultPlan.Name {
			return &s.defaultPlan, nil
		}
		if s.plan.Name == name {
			return &s.plan, nil
		}
		return nil, appTypes.ErrPlanNotFound
	}
	s.mockService.AppQuota.OnGet = func(_ quota.QuotaItem) (*quota.Quota, error) {
		return &quota.UnlimitedQuota, nil
	}
	s.mockService.TeamQuota.OnGet = func(_ quota.QuotaItem) (*quota.Quota, error) {
		return &quota.UnlimitedQuota, nil
	}
	s.mockService.Pool.OnServices = func(pool string) ([]string, error) {
		return []string{
			"my",
			"mysql",
			"healthcheck",
		}, nil
	}
	s.builder.OnBuild = func(p provision.BuilderDeploy, app provision.App, evt *event.Event, opts *builder.BuildOpts) (appTypes.AppVersion, error) {
		version, err := servicemanager.AppVersion.NewAppVersion(context.TODO(), appTypes.NewVersionArgs{
			App: app,
		})
		if err != nil {
			return nil, err
		}
		return version, version.CommitBuildImage()
	}
}
