package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/mint"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/toolapprove"
	"github.com/hivecommons/hive/pkg/trajectory"
	"github.com/hivecommons/hive/pkg/watchdog"
)

// wireSpokeSubsystems runs the spoke-mode subsystem wiring after the common
// process, config path, logging, and hub-mode preamble has completed. It keeps
// the original startup order byte-for-byte inside the spoke path while main()
// becomes an ordered dispatcher. Follow-up phase-2c PRs should peel contiguous
// sections from this function into narrower wireX constructors.
type spokeWire struct {
	configPath                 string
	logger                     *slog.Logger
	startTime                  time.Time
	cfg                        *config.Config
	ctx                        context.Context
	cancel                     context.CancelFunc
	preShutdownHooks           shutdownHooks
	ghAuth                     githubAuth
	ghClient                   *github.Client
	appAuth                    *github.AppAuth
	appAuthFailure             string
	appAuthState               github.AppAuthState
	gov                        *governor.Governor
	sched                      *scheduler.Scheduler
	promptFetcher              promptsrc.Fetcher
	defFetcher                 defsrc.Fetcher
	definitionResolver         *defsrc.Resolver
	pendingTokenSeed           []dashboard.TokenSparklineEntry
	pendingFactSeed            []dashboard.FactHistoryEntry
	pendingCostSeed            []dashboard.CostHistoryEntry
	pendingBudgetWindowSeed    []collect.BudgetWindowEntry
	pendingConvergenceSoakSeed []dashboard.ConvergenceSoakEntry
	pendingTrendSeed           []dashboard.TrendHistoryEntry
	primer                     *knowledge.Primer
	notifier                   *notify.Notifier
	acmmLevel                  int
	githubAppDiag              string
	githubAppState             github.AppAuthState
	githubAppRequired          bool
	repoTargetMisconfigured    func() bool
	repoTargetIssueMessage     func() string
	advisoryIssues             map[string]int
	advisoryStore              *advisory.Store
	policyDir                  string
	projectCtx                 agent.ProjectContext
	agentMgr                   *agent.Manager
	approvalDesk               *toolapprove.Desk
	approvalInbox              *toolapprove.Inbox
	agentMinter                *mint.AgentMinter
	saved                      *snapshot.PersistedState
	dashSrv                    *dashboard.Server
	beadStores                 map[string]*beads.Store
	beadStoreLoadFailures      int
	tokenCollector             *tokens.Collector
	metricsCollector           *dashboard.MetricsCollector
	fleetStatsCollector        *collect.FleetStatsCollector
	activityCollector          *collect.ActivityCollector
	repoCostCollector          *collect.RepoCostCollector
	lastActionable             atomic.Pointer[github.ActionableResult]
	knowledgeAPI               *knowledge.KnowledgeAPI
	gitSyncer                  *knowledge.GitSyncer
	beadSynth                  *knowledge.BeadSynthesizer
	promotionScheduler         *knowledge.PromotionScheduler
	nousState                  *dashboard.NousState
	inceptionEngine            *knowledge.InceptionEngine
	rotationMgr                *rotation.Manager
	quotaReadingPublisher      *rotation.Manager
	wd                         *watchdog.Reconciler
	configWatcher              *config.Watcher
	onDemandFromPack           map[string]bool
	githubProxy                *proxy.GitHubProxy
	reporterName               string
	processStartedAt           time.Time
	lastAutoMergeSweep         time.Time
	lastTaskListSweep          time.Time
	refreshDashboard           func()
	hubURL                     string
	mutationBoundary           effects.Boundary
	mutationStats              *effects.Recorder
	trajLane                   *trajectory.Lane
	replanLane                 *planning.ReplanLane
	retroLane                  *retro.Lane
	cleanups                   []func()
}

const spokeStatePath = "/data/hive-state.json"

func wireSpokeSubsystems(configPath string, logger *slog.Logger, startTime time.Time) {
	w := &spokeWire{configPath: configPath, logger: logger, startTime: startTime}
	defer w.runCleanups()
	w.run()
}

func (w *spokeWire) addCleanup(fn func()) {
	if fn != nil {
		w.cleanups = append(w.cleanups, fn)
	}
}

func (w *spokeWire) runCleanups() {
	for i := len(w.cleanups) - 1; i >= 0; i-- {
		w.cleanups[i]()
	}
}

func (w *spokeWire) run() {
	w.wireSpokeConfigAndSignals()
	w.wireSpokeAuthGovernor()
	w.wireMutationBoundary()
	w.wireSpokeAgentsAndRequests()
	w.wireSpokeStateDashboard()
	w.wireSpokeMetricsAndKnowledgeAPI()
	w.wireSpokeKnowledgeSources()
	w.wireSpokeManagersAndLinear()
	w.wireSpokeAppCallbacks()
	w.wireSpokeConfigReloadAndHooks()
	w.wireSpokeProxyReadyAndLaunch()
	w.wireHubHeartbeat()
	w.wireSpokeLanesAndLoop()
}
