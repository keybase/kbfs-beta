// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

//
// globals
//
//   All of the global objects in the libkb namespace that are shared
//   and mutated across various source files are here.  They are
//   accessed like `G.Log` or `G.Env`.  They're kept
//   under the `G` namespace to better keep track of them all.
//
//   The globals are built up gradually as the process comes up.
//   At first, we only have a logger, but eventually we add
//   command-line flags, configuration and environment, and accordingly,
//   might actually go back and change the Logger.

package libkb

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
)

type ShutdownHook func() error

type GlobalContext struct {
	Log               logger.Logger  // Handles all logging
	Env               *Env           // Env variables, cmdline args & config
	Keyrings          *Keyrings      // Gpg Keychains holding keys
	API               API            // How to make a REST call to the server
	ResolveCache      *ResolveCache  // cache of resolve results
	LocalDb           *JSONLocalDb   // Local DB for cache
	MerkleClient      *MerkleClient  // client for querying server's merkle sig tree
	XAPI              ExternalAPI    // for contacting Twitter, Github, etc.
	Output            io.Writer      // where 'Stdout'-style output goes
	ProofCache        *ProofCache    // where to cache proof results
	GpgClient         *GpgCLI        // A standard GPG-client (optional)
	ShutdownHooks     []ShutdownHook // on shutdown, fire these...
	SocketInfo        Socket         // which socket to bind/connect to
	socketWrapperMu   sync.RWMutex
	SocketWrapper     *SocketWrapper     // only need one connection per
	LoopbackListener  *LoopbackListener  // If we're in loopback mode, we'll connect through here
	XStreams          *ExportedStreams   // a table of streams we've exported to the daemon (or vice-versa)
	Timers            *TimerSet          // Which timers are currently configured on
	IdentifyCache     *IdentifyCache     // cache of IdentifyOutcomes
	UserCache         *UserCache         // cache of Users
	UI                UI                 // Interact with the UI
	Service           bool               // whether we're in server mode
	shutdownOnce      sync.Once          // whether we've shut down or not
	loginStateMu      sync.RWMutex       // protects loginState pointer, which gets destroyed on logout
	loginState        *LoginState        // What phase of login the user's in
	ConnectionManager *ConnectionManager // keep tabs on all active client connections
	NotifyRouter      *NotifyRouter      // How to route notifications
	UIRouter          UIRouter           // How to route UIs
	ExitCode          keybase1.ExitCode  // Value to return to OS on Exit()
}

func NewGlobalContext() *GlobalContext {
	return &GlobalContext{
		Log: logger.New("keybase", ErrorWriter()),
	}
}

var G *GlobalContext

func init() {
	G = NewGlobalContext()
}

func (g *GlobalContext) SetCommandLine(cmd CommandLine) { g.Env.SetCommandLine(cmd) }

func (g *GlobalContext) SetUI(u UI) { g.UI = u }

func (g *GlobalContext) Init() {
	g.Env = NewEnv(nil, nil)
	g.Service = false
	g.createLoginState()
}

func (g *GlobalContext) SetService() {
	g.Service = true
	g.ConnectionManager = NewConnectionManager()
	g.NotifyRouter = NewNotifyRouter(g)
}

func (g *GlobalContext) SetUIRouter(u UIRouter) {
	g.UIRouter = u
}

// requires lock on loginStateMu before calling
func (g *GlobalContext) createLoginStateLocked() {
	if g.loginState != nil {
		g.loginState.Shutdown()
	}
	g.loginState = NewLoginState(g)
}

func (g *GlobalContext) createLoginState() {
	if g.loginState != nil {
		g.loginState.Shutdown()
	}
	g.loginState = NewLoginState(g)
}

func (g *GlobalContext) LoginState() *LoginState {
	g.loginStateMu.RLock()
	defer g.loginStateMu.RUnlock()

	return g.loginState
}

func (g *GlobalContext) Logout() error {
	g.loginStateMu.Lock()
	defer g.loginStateMu.Unlock()
	if err := g.loginState.Logout(); err != nil {
		return err
	}

	if g.IdentifyCache != nil {
		g.IdentifyCache.Shutdown()
	}
	if g.UserCache != nil {
		g.UserCache.Shutdown()
	}
	g.IdentifyCache = NewIdentifyCache()
	g.UserCache = NewUserCache(g.Env.GetUserCacheMaxAge())

	// get a clean LoginState:
	g.createLoginStateLocked()

	return nil
}

func (g *GlobalContext) ConfigureLogging() error {
	g.Log.Configure(g.Env.GetLogFormat(), g.Env.GetDebug(),
		g.Env.GetLogFile())
	g.Output = os.Stdout
	return nil
}

func (g *GlobalContext) PushShutdownHook(sh ShutdownHook) {
	g.ShutdownHooks = append(g.ShutdownHooks, sh)
}

func (g *GlobalContext) ConfigureConfig() error {
	c := NewJSONConfigFile(g, g.Env.GetConfigFilename())
	err := c.Load(false)
	if err != nil {
		return err
	}
	if err = c.Check(); err != nil {
		return err
	}
	g.Env.SetConfig(*c)
	g.Env.SetConfigWriter(c)
	return nil
}

func (g *GlobalContext) ConfigReload() error {
	return g.ConfigureConfig()
}

func (g *GlobalContext) ConfigureTimers() error {
	g.Timers = NewTimerSet(g)
	return nil
}

func (g *GlobalContext) ConfigureKeyring() error {
	g.Keyrings = NewKeyrings(g)
	return nil
}

func VersionMessage(linefn func(string)) {
	linefn(fmt.Sprintf("Keybase CLI %s", VersionString()))
	linefn(fmt.Sprintf("- Built with %s", runtime.Version()))
	linefn("- Visit https://keybase.io for more details")
}

func (g *GlobalContext) StartupMessage() {
	VersionMessage(func(s string) { g.Log.Debug(s) })
}

func (g *GlobalContext) ConfigureAPI() error {
	iapi, xapi, err := NewAPIEngines(g)
	if err != nil {
		return fmt.Errorf("Failed to configure API access: %s", err)
	}
	g.API = iapi
	g.XAPI = xapi
	return nil
}

func (g *GlobalContext) ConfigureCaches() error {
	g.ResolveCache = NewResolveCache()
	g.IdentifyCache = NewIdentifyCache()
	g.UserCache = NewUserCache(g.Env.GetUserCacheMaxAge())
	g.ProofCache = NewProofCache(g, g.Env.GetProofCacheSize())

	// We consider the local DB as a cache; it's caching our
	// fetches from the server after all (and also our cryptographic
	// checking).
	g.LocalDb = NewJSONLocalDb(NewLevelDb(g))
	return g.LocalDb.Open()
}

func (g *GlobalContext) ConfigureMerkleClient() error {
	g.MerkleClient = NewMerkleClient(g)
	return nil
}

func (g *GlobalContext) ConfigureExportedStreams() error {
	g.XStreams = NewExportedStreams()
	return nil
}

// Shutdown is called exactly once per-process and does whatever
// cleanup is necessary to shut down the server.
func (g *GlobalContext) Shutdown() error {
	var err error
	didShutdown := false

	// Wrap in a Once.Do so that we don't inadvertedly
	// run this code twice.
	g.shutdownOnce.Do(func() {
		G.Log.Debug("Calling shutdown first time through")
		didShutdown = true

		epick := FirstErrorPicker{}

		if g.NotifyRouter != nil {
			g.NotifyRouter.Shutdown()
		}

		if g.UIRouter != nil {
			g.UIRouter.Shutdown()
		}

		if g.ConnectionManager != nil {
			g.ConnectionManager.Shutdown()
		}

		if g.UI != nil {
			epick.Push(g.UI.Shutdown())
		}
		if g.LocalDb != nil {
			epick.Push(g.LocalDb.Close())
		}
		if g.LoginState() != nil {
			epick.Push(g.LoginState().Shutdown())
		}

		if g.IdentifyCache != nil {
			g.IdentifyCache.Shutdown()
		}
		if g.UserCache != nil {
			g.UserCache.Shutdown()
		}

		for _, hook := range g.ShutdownHooks {
			epick.Push(hook())
		}

		err = epick.Error()

		g.Log.Debug("exiting shutdown code=%d; err=%v", g.ExitCode, err)
	})

	// Make a little bit of a statement if we wind up here a second time
	// (which is a bug).
	if !didShutdown {
		G.Log.Debug("Skipped shutdown on second call")
	}

	return err
}

func (u Usage) UseKeyring() bool {
	return u.KbKeyring || u.GpgKeyring
}

func (g *GlobalContext) ConfigureCommand(line CommandLine, cmd Command) error {
	usage := cmd.GetUsage()
	return g.Configure(line, usage)
}

func (g *GlobalContext) Configure(line CommandLine, usage Usage) error {
	g.SetCommandLine(line)
	err := g.ConfigureLogging()
	if err != nil {
		return err
	}
	return g.ConfigureUsage(usage)
}

func (g *GlobalContext) ConfigureUsage(usage Usage) error {
	var err error

	if usage.Config {
		if err = g.ConfigureConfig(); err != nil {
			return err
		}
	}
	if usage.UseKeyring() {
		if err = g.ConfigureKeyring(); err != nil {
			return err
		}
	}
	if usage.API {
		if err = g.ConfigureAPI(); err != nil {
			return err
		}
	}
	if usage.Socket || !g.Env.GetStandalone() {
		if err = g.ConfigureSocketInfo(); err != nil {
			return err
		}
	}

	if err = g.ConfigureExportedStreams(); err != nil {
		return err
	}

	if err = g.ConfigureCaches(); err != nil {
		return err
	}

	if err = g.ConfigureMerkleClient(); err != nil {
		return err
	}
	if g.UI != nil {
		if err = g.UI.Configure(); err != nil {
			return err
		}
	}

	if err = g.ConfigureTimers(); err != nil {
		return err
	}

	return nil
}

func (g *GlobalContext) OutputString(s string) {
	g.Output.Write([]byte(s))
}

func (g *GlobalContext) OutputBytes(b []byte) {
	g.Output.Write(b)
}

func (g *GlobalContext) GetGpgClient() *GpgCLI {
	if g.GpgClient == nil {
		g.GpgClient = NewGpgCLI(g, nil)
	}
	return g.GpgClient
}

func (g *GlobalContext) GetMyUID() keybase1.UID {
	var uid keybase1.UID

	g.LoginState().LocalSession(func(s *Session) {
		uid = s.GetUID()
	}, "G - GetMyUID - GetUID")
	if uid.Exists() {
		return uid
	}

	return g.Env.GetUID()
}

func (g *GlobalContext) ConfigureSocketInfo() (err error) {
	g.SocketInfo, err = NewSocket(g)
	return err
}

// Contextified objects have explicit references to the GlobalContext,
// so that G can be swapped out for something else.  We're going to incrementally
// start moving objects over to this system.
type Contextified struct {
	g *GlobalContext
}

func (c Contextified) G() *GlobalContext {
	if c.g != nil {
		return c.g
	}
	return G
}

func (c Contextified) GStrict() *GlobalContext {
	return c.g
}

func (c *Contextified) SetGlobalContext(g *GlobalContext) { c.g = g }

func NewContextified(gc *GlobalContext) Contextified {
	return Contextified{g: gc}
}

type Contexitifier interface {
	G() *GlobalContext
}
