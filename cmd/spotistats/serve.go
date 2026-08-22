package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/neovasili/spotistats/internal/api"
	"github.com/neovasili/spotistats/internal/config"
)

// DefaultServeAddr is the address the frontend dev server proxies to in Mode B of
// docs/SPECS.md 7.4. Bound to loopback rather than all interfaces: this serves a personal
// dataset from a developer's laptop and has no authentication.
const DefaultServeAddr = "127.0.0.1:8787"

func runServe(ctx context.Context, args []string) error {
	fs := newFlagSet("serve", "serve [flags]")
	addr := fs.String("addr", DefaultServeAddr, "address to listen on")
	dataDir := fs.String("data", "", "directory to serve under /data/ (default: "+DefaultDataDir+" if it exists)")
	webDir := fs.String("web", "", "directory to serve as the site root (a built frontend bundle)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	deps, err := config.Build(ctx, cfg, config.BuildOptions{NeedStore: true})
	if err != nil {
		return err
	}
	cal, err := cfg.Calendar()
	if err != nil {
		return err
	}

	// The same handler the query Lambda wraps, so what the frontend develops against is what
	// production serves.
	apiHandler, err := api.New(api.Config{
		Store:    deps.Store,
		Calendar: cal,
		Logger:   deps.Logger,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle(api.BasePath+"/", apiHandler)

	// Fall back to wherever `spotistats rollup` writes, so `rollup` then `serve` works with no
	// flags at all.
	if *dataDir == "" {
		if _, err := os.Stat(DefaultDataDir); err == nil {
			*dataDir = DefaultDataDir
		}
	}

	// The dashboard reads data/dashboard.json from its own origin rather than the API, so the
	// local server has to serve it too or Mode B cannot render the dashboard at all.
	if *dataDir != "" {
		abs, aerr := filepath.Abs(*dataDir)
		if aerr != nil {
			return aerr
		}
		if _, serr := os.Stat(abs); serr != nil {
			return fmt.Errorf("data directory %s: %w", abs, serr)
		}
		mux.Handle("/data/", noCache(http.StripPrefix("/data/", http.FileServer(http.Dir(abs)))))
	} else {
		mux.HandleFunc("/data/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":` +
				`"no snapshot directory configured; pass -data <dir>. Snapshot rendering ` +
				`arrives with the rollup Lambda in milestone 7."}}` + "\n"))
		})
	}

	if *webDir != "" {
		abs, aerr := filepath.Abs(*webDir)
		if aerr != nil {
			return aerr
		}
		mux.Handle("/", spaHandler(abs))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "spotistats local API\n\n"+
				"  API:    %s%s/meta\n"+
				"  health: %s%s/health\n\n"+
				"Point the frontend dev server here:\n"+
				"  cd web && VITE_API_TARGET=http://%s npm run dev\n",
				"http://"+*addr, api.BasePath, "http://"+*addr, api.BasePath, *addr)
		})
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	heading("Serving the query API")
	bullet("address:  http://%s%s", *addr, api.BasePath)
	bullet("table:    %s", cfg.TableName)
	if cfg.DDBEndpoint != "" {
		bullet("dynamodb: %s", cfg.DDBEndpoint)
	} else {
		bullet("dynamodb: AWS (%s)", cfg.Region)
	}
	if *dataDir != "" {
		bullet("data:     %s", *dataDir)
	}
	if *webDir != "" {
		bullet("web:      %s", *webDir)
	}
	fmt.Printf("\nFrontend dev server:\n  cd web && VITE_API_TARGET=http://%s npm run dev\n\n"+
		"Ctrl-C to stop.\n", *addr)

	// Shut down on context cancellation so Ctrl-C is clean rather than abrupt.
	errCh := make(chan error, 1)
	go func() {
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
			return
		}
		errCh <- nil
	}()

	select {
	case serr := <-errCh:
		return serr
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			return serr
		}
		fmt.Println("\nstopped")
		return nil
	}
}

// noCache prevents the browser caching local snapshots, so re-rendering them shows up on
// reload instead of requiring a hard refresh.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// spaHandler serves a built frontend bundle, falling back to index.html for client-side
// routes. It mirrors the CloudFront behaviour in docs/SPECS.md 9.1, where 403 and 404 from
// the S3 origin rewrite to index.html -- but only for the site, never for /api/*, which is
// why this is registered after the API routes.
func spaHandler(root string) http.Handler {
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(root, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		index := filepath.Join(root, "index.html")
		if _, err := os.Stat(index); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}
