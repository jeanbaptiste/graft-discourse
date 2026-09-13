// Command bridge runs the Discourse <-> Graft bridge: it mirrors Discourse
// posts to repo issues/patches as ActivityPub replies, and repo comments
// back into Discourse, using Graft's public ActivityPub surface.
package main

import (
	"context"
	"crypto/rsa"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"graftdiscourse/internal/admin"
	"graftdiscourse/internal/ap"
	"graftdiscourse/internal/bridge"
	"graftdiscourse/internal/config"
	"graftdiscourse/internal/discourse"
	"graftdiscourse/internal/graft"
	"graftdiscourse/internal/netguard"
	"graftdiscourse/internal/state"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to the JSON config file")
	once := flag.Bool("once", false, "run a single pass and exit")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		log.Error("load state", "err", err)
		os.Exit(1)
	}

	actor, priv, err := ensureActor(cfg, st)
	if err != nil {
		log.Error("ensure actor", "err", err)
		os.Exit(1)
	}
	signFn := func(req *http.Request, body []byte) error {
		return ap.SignRequest(req, body, actor.KeyID(), priv)
	}

	// Graft is reached over the public internet and hands us actor inbox
	// URLs off the wire, so its client refuses private addresses and
	// plaintext. Discourse may be a self-hosted internal service, so it is
	// allowed private addresses, but its API key is stripped on any
	// cross-host redirect.
	graftPolicy := netguard.Options{
		AllowPrivate: false,
		// Always HTTPS, independent of cfg.AllowInsecureHTTP: that flag
		// exists only to let a self-hosted Discourse sit on plain HTTP
		// internally. Graft is reached over the public internet and its
		// own SSRF guard rejects a plaintext actor/inbox URL outright, so
		// relaxing this here would only break interop, never help it.
		AllowHTTP:        false,
		SensitiveHeaders: []string{"Api-Key", "Api-Username"},
	}
	discoursePolicy := netguard.Options{
		AllowPrivate:     true,
		AllowHTTP:        cfg.AllowInsecureHTTP,
		SensitiveHeaders: []string{"Api-Key", "Api-Username"},
	}

	graftHost := hostOf(cfg.Graft.BaseURL)
	gc := graft.New(graft.Config{
		BaseURL:  cfg.Graft.BaseURL,
		ActorURL: actor.ActorURL(),
		KeyID:    actor.KeyID(),
		HTTP:     graftPolicy.Client(),
		Sign:     signFn,
		Validate: graftPolicy.Validate,
	})
	dc := discourse.New(cfg.Discourse.BaseURL, cfg.Discourse.APIKey, cfg.Discourse.APIUsername, discoursePolicy.Client())

	b := &bridge.Bridge{
		GraftHost: graftHost,
		Series:    cfg.Graft.Series,
		MaxPosts:  cfg.Discourse.MaxPosts,
		Opts: bridge.Options{
			Explicit:             explicitMappings(cfg.Mappings),
			AllowTitleMatching:   cfg.AllowTitleMatching,
			AllowedCategories:    categorySet(cfg.AllowedCategories),
			MaxContentRunes:      cfg.MaxContentRunes,
			MaxDeliveriesPerPass: cfg.MaxDeliveriesPerPass,
		},
		Discourse: dc,
		Graft:     gc,
		State:     st,
		Log:       log,
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newMux(actor, b, cfg.AdminToken, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	go func() {
		log.Info("serving bridge actor", "listen", cfg.Listen, "actor", actor.ActorURL())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("actor server", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runPass := func() {
		pctx, cancel := context.WithTimeout(ctx, cfg.PassTimeout.D())
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				log.Error("sync pass panicked", "panic", r)
			}
		}()
		if err := b.RunOnce(pctx); err != nil {
			log.Error("sync pass", "err", err)
		}
	}

	if *once {
		runPass()
		shutdown(srv, log)
		return
	}

	interval := cfg.PollInterval.D()
	log.Info("bridge started", "interval", interval.String(), "series", strings.Join(cfg.Graft.Series, ","))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		runPass()
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			shutdown(srv, log)
			return
		case <-ticker.C:
		}
	}
}

func shutdown(srv *http.Server, log *slog.Logger) {
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(cctx); err != nil {
		log.Error("actor server shutdown", "err", err)
	}
}

// ensureActor loads (or generates and persists) the bridge actor keypair.
func ensureActor(cfg *config.Config, st *state.State) (*ap.ActorServer, *rsa.PrivateKey, error) {
	kp, ok := st.KeyPair(cfg.ActorName)
	if !ok {
		privPEM, pubPEM, err := ap.GenerateKeyPair()
		if err != nil {
			return nil, nil, err
		}
		kp = state.KeyPair{PrivatePEM: privPEM, PublicPEM: pubPEM}
		if err := st.SetKeyPair(cfg.ActorName, kp); err != nil {
			return nil, nil, err
		}
	}
	priv, err := ap.ParsePrivateKey(kp.PrivatePEM)
	if err != nil {
		return nil, nil, err
	}
	actor := &ap.ActorServer{
		BaseURL:   strings.TrimRight(cfg.PublicBaseURL, "/"),
		Name:      cfg.ActorName,
		PublicPEM: kp.PublicPEM,
		RepoURL:   cfg.RepoURL,
	}
	return actor, priv, nil
}

// newMux mounts the public actor surface and, when an admin token is set,
// the authenticated admin API. The env var overrides the config so the
// token need not be stored on disk.
func newMux(actor *ap.ActorServer, b admin.Binder, cfgToken string, log *slog.Logger) http.Handler {
	token := cfgToken
	if v := os.Getenv("GRAFT_BRIDGE_ADMIN_TOKEN"); v != "" {
		token = v
	}
	mux := http.NewServeMux()
	actor.Register(mux)
	if token != "" {
		(&admin.Server{Token: token, Binder: b, Log: log}).Register(mux)
		log.Info("admin API enabled", "path", "/admin/mappings")
	}
	return mux
}

func explicitMappings(ms []config.Mapping) map[int64]string {
	out := make(map[int64]string, len(ms))
	for _, m := range ms {
		out[m.TopicID] = m.NoteURI
	}
	return out
}

func categorySet(ids []int64) map[int64]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func hostOf(base string) string {
	s := base
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}
