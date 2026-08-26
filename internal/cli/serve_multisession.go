package cli

import (
	"flag"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/serve"
)

func registerServeCapabilityFlags(fs *flag.FlagSet) {
	_ = fs.Bool("session-events", false, "tag session events and finish switched-away turns in background (reasonix-serve-caps-20260822c)")
	_ = fs.Bool("detached-heal", false, "retire background sessions after provider credential-channel repair")
}

func newServeBootstrap() (*serve.Broadcaster, *serve.SessionTagSink, *config.Config) {
	bc := serve.NewBroadcaster()
	cfg, _ := config.Load()
	return bc, serve.NewSessionTagSink(bc), cfg
}

func newCLIMultiSessionServer(ctrl *control.Controller, bc *serve.Broadcaster, tag *serve.SessionTagSink, cfg config.ServeConfig, leases *control.SessionLeaseKeeper) *serve.Server {
	tag.SetPath(ctrl.SessionPath())
	srv := serve.New(ctrl, bc, cfg)
	srv.RegisterSessionTag(ctrl, tag)
	_ = srv.SetSessionLeases(leases)
	return srv
}
